package storage

import "strings"

type replicaTemplate struct {
	domain                 Domain
	table                  TableSpec
	cols, keys             []ColumnSpec
	insert, update, delete string
}

// 只编译固定登记表，不接受日志中的 SQL 或标识符。
var replicaTemplates = func() map[uint16]replicaTemplate {
	out := map[uint16]replicaTemplate{}
	for _, domain := range []Domain{MainDomain, HistoryDomain} {
		for _, table := range Tables(domain) {
			cols := columns(table)
			var where, set []string
			for _, key := range table.PK {
				where = append(where, "`"+key+"`=?")
			}
			for _, c := range cols {
				set = append(set, "`"+c.Name+"`=?")
			}
			out[table.ID] = replicaTemplate{domain: domain, table: table, cols: cols, keys: primaryColumns(table), insert: "INSERT INTO `" + table.Name + "`(" + columnNames(cols) + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")", update: "UPDATE `" + table.Name + "` SET " + strings.Join(set, ",") + " WHERE " + strings.Join(where, " AND "), delete: "DELETE FROM `" + table.Name + "` WHERE " + strings.Join(where, " AND ")}
		}
	}
	return out
}()
