package db

import (
	"context"
	"fmt"
	"strings"
)

// schema records which columns exist, so queries adapt to the macOS version
// instead of hard-coding version gates.
type schema map[string]map[string]bool

func probeSchema(ctx context.Context, d *DB, tables ...string) (schema, error) {
	s := schema{}
	for _, t := range tables {
		rows, err := d.sql.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", t, err)
		}
		cols := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			cols[name] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			return nil, fmt.Errorf("table %s not found in %s", t, d.path)
		}
		s[t] = cols
	}
	return s, nil
}

func (s schema) has(table, col string) bool { return s[table][col] }

type colKind int

const (
	kInt colKind = iota
	kStr
	kBlob
)

// selectList renders column expressions for alias.col, substituting a typed
// default for columns that do not exist so scan targets stay fixed.
type selectList struct {
	s     schema
	table string
	alias string
	exprs []string
}

func (l *selectList) add(col string, k colKind) {
	if l.s.has(l.table, col) {
		ref := l.alias + "." + col
		switch k {
		case kInt:
			ref = "IFNULL(" + ref + ", 0)"
		case kStr:
			ref = "IFNULL(" + ref + ", '')"
		}
		l.exprs = append(l.exprs, ref)
		return
	}
	switch k {
	case kInt:
		l.exprs = append(l.exprs, "0")
	case kStr:
		l.exprs = append(l.exprs, "''")
	default:
		l.exprs = append(l.exprs, "NULL")
	}
}

func (l *selectList) String() string { return strings.Join(l.exprs, ", ") }
