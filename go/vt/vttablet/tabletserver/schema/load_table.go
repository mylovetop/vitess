/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package schema

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	log "github.com/golang/glog"

	"github.com/youtube/vitess/go/vt/sqlparser"
	"github.com/youtube/vitess/go/vt/vttablet/tabletserver/connpool"
	"github.com/youtube/vitess/go/vt/vttablet/tabletserver/tabletenv"

	querypb "github.com/youtube/vitess/go/vt/proto/query"
)

// LoadTable creates a Table from the schema info in the database.
func LoadTable(conn *connpool.DBConn, tableName string, tableType string, comment string) (*Table, error) {
	ta := NewTable(tableName)
	sqlTableName := sqlparser.String(ta.Name)
	if err := fetchColumns(ta, conn, sqlTableName); err != nil {
		return nil, err
	}
	if err := fetchIndexes(ta, conn, sqlTableName); err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(comment, "vitess_sequence"):
		ta.Type = Sequence
		ta.SequenceInfo = &SequenceInfo{}
	case strings.Contains(comment, "vitess_message"):
		if err := loadMessageInfo(ta, comment); err != nil {
			return nil, err
		}
		ta.Type = Message
	}
	return ta, nil
}

func fetchColumns(ta *Table, conn *connpool.DBConn, sqlTableName string) error {
	qr, err := conn.Exec(tabletenv.LocalContext(), fmt.Sprintf("select * from %s where 1 != 1", sqlTableName), 0, true)
	if err != nil {
		return err
	}
	fieldTypes := make(map[string]querypb.Type, len(qr.Fields))
	// TODO(sougou): Store the full field info in the schema.
	for _, field := range qr.Fields {
		fieldTypes[field.Name] = field.Type
	}
	columns, err := conn.Exec(tabletenv.LocalContext(), fmt.Sprintf("describe %s", sqlTableName), 10000, false)
	if err != nil {
		return err
	}
	for _, row := range columns.Rows {
		name := row[0].String()
		columnType, ok := fieldTypes[name]
		if !ok {
			log.Warningf("Table: %s, column %s not found in select list, skipping.", ta.Name, name)
			continue
		}
		ta.AddColumn(name, columnType, row[4], row[5].String())
	}
	return nil
}

func fetchIndexes(ta *Table, conn *connpool.DBConn, sqlTableName string) error {
	indexes, err := conn.Exec(tabletenv.LocalContext(), fmt.Sprintf("show index from %s", sqlTableName), 10000, false)
	if err != nil {
		return err
	}
	var currentIndex *Index
	currentName := ""
	for _, row := range indexes.Rows {
		indexName := row[2].String()
		if currentName != indexName {
			currentIndex = ta.AddIndex(indexName)
			currentName = indexName
		}
		var cardinality uint64
		if !row[6].IsNull() {
			cardinality, err = strconv.ParseUint(row[6].String(), 0, 64)
			if err != nil {
				log.Warningf("%s", err)
			}
		}
		currentIndex.AddColumn(row[4].String(), cardinality)
	}
	ta.Done()
	return nil
}

func loadMessageInfo(ta *Table, comment string) error {
	findCols := map[string]struct{}{
		"id":             {},
		"time_scheduled": {},
		"time_next":      {},
		"epoch":          {},
		"time_created":   {},
		"time_acked":     {},
	}

	// orderedColumns are necessary to ensure that they
	// get added in the correct order to fields if they
	// need to be returned with the stream.
	orderedColumns := []string{
		"id",
		"time_scheduled",
		"time_next",
		"epoch",
		"time_created",
		"time_acked",
	}

	ta.MessageInfo = &MessageInfo{}
	// Extract keyvalues.
	keyvals := make(map[string]string)
	inputs := strings.Split(comment, ",")
	for _, input := range inputs {
		kv := strings.Split(input, "=")
		if len(kv) != 2 {
			continue
		}
		keyvals[kv[0]] = kv[1]
	}
	var err error
	if ta.MessageInfo.AckWaitDuration, err = getDuration(keyvals, "vt_ack_wait"); err != nil {
		return err
	}
	if ta.MessageInfo.PurgeAfterDuration, err = getDuration(keyvals, "vt_purge_after"); err != nil {
		return err
	}
	if ta.MessageInfo.BatchSize, err = getNum(keyvals, "vt_batch_size"); err != nil {
		return err
	}
	if ta.MessageInfo.CacheSize, err = getNum(keyvals, "vt_cache_size"); err != nil {
		return err
	}
	if ta.MessageInfo.PollInterval, err = getDuration(keyvals, "vt_poller_interval"); err != nil {
		return err
	}
	for _, col := range orderedColumns {
		num := ta.FindColumn(sqlparser.NewColIdent(col))
		if num == -1 {
			return fmt.Errorf("%s missing from message table: %s", col, ta.Name.String())
		}

		// id and time_scheduled must be the first two columns.
		if col == "id" || col == "time_scheduled" {
			ta.MessageInfo.Fields = append(ta.MessageInfo.Fields, &querypb.Field{
				Name: ta.Columns[num].Name.String(),
				Type: ta.Columns[num].Type,
			})
		}
	}

	// Store the position of the id column in the PK
	// list. This is required to handle arbitrary updates.
	// In such cases, we have to be able to identify the
	// affected id and invalidate the message cache.
	ta.MessageInfo.IDPKIndex = -1
	for i, j := range ta.PKColumns {
		if ta.Columns[j].Name.EqualString("id") {
			ta.MessageInfo.IDPKIndex = i
			break
		}
	}
	if ta.MessageInfo.IDPKIndex == -1 {
		return fmt.Errorf("id column is not part of the primary key for message table: %s", ta.Name.String())
	}

	// Load user-defined columns. Any "unrecognized" column is user-defined.
	for _, c := range ta.Columns {
		if _, ok := findCols[c.Name.Lowered()]; ok {
			continue
		}

		ta.MessageInfo.Fields = append(ta.MessageInfo.Fields, &querypb.Field{
			Name: c.Name.String(),
			Type: c.Type,
		})
	}
	return nil
}

func getDuration(in map[string]string, key string) (time.Duration, error) {
	sv := in[key]
	if sv == "" {
		return 0, fmt.Errorf("Attribute %s not specified for message table", key)
	}
	v, err := strconv.ParseFloat(sv, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(v * 1e9), nil
}

func getNum(in map[string]string, key string) (int, error) {
	sv := in[key]
	if sv == "" {
		return 0, fmt.Errorf("Attribute %s not specified for message table", key)
	}
	v, err := strconv.Atoi(sv)
	if err != nil {
		return 0, err
	}
	return v, nil
}
