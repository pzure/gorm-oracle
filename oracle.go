package oracle

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pzure/gorm-oracle/clauses"
	oracle "github.com/sijms/go-ora/v2"
	"github.com/thoas/go-funk"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

type Config struct {
	DriverName        string
	DSN               string
	Conn              *sql.DB
	DefaultStringSize uint
}

type Dialector struct {
	*Config
}

func BuildUrl(server string, port int, service, user, password string, options map[string]string) string {
	return oracle.BuildUrl(server, port, service, user, password, options)
}
func Open(dsn string) gorm.Dialector {
	return &Dialector{Config: &Config{DSN: dsn}}
}

func New(config Config) gorm.Dialector {
	return &Dialector{Config: &config}
}

func (d Dialector) DummyTableName() string {
	return "DUAL"
}

func (d Dialector) Name() string {
	return "oracle"
}

func (d Dialector) Initialize(db *gorm.DB) (err error) {
	d.DefaultStringSize = 1024

	// register callbacks
	//callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{WithReturning: true})
	callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{LastInsertIDReversed: true})

	d.DriverName = "oracle"

	if d.Conn != nil {
		db.ConnPool = d.Conn
	} else {
		db.ConnPool, err = sql.Open(d.DriverName, d.DSN)
	}

	if err = db.Callback().Create().Replace("gorm:create", Create); err != nil {
		return
	}

	for k, v := range d.ClauseBuilders() {
		db.ClauseBuilders[k] = v
	}
	return
}

func (d Dialector) ClauseBuilders() map[string]clause.ClauseBuilder {
	return map[string]clause.ClauseBuilder{
		"LIMIT": d.RewriteLimit,
		"WHERE": d.RewriteWhere,
	}
}

func (d Dialector) RewriteWhere(c clause.Clause, builder clause.Builder) {
	if where, ok := c.Expression.(clause.Where); ok {
		builder.WriteString(" WHERE ")

		// Switch position if the first query expression is a single Or condition
		for idx, expr := range where.Exprs {
			if v, ok := expr.(clause.OrConditions); !ok || len(v.Exprs) > 1 {
				if idx != 0 {
					where.Exprs[0], where.Exprs[idx] = where.Exprs[idx], where.Exprs[0]
				}
				break
			}
		}

		wrapInParentheses := false
		for idx, expr := range where.Exprs {
			if idx > 0 {
				if v, ok := expr.(clause.OrConditions); ok && len(v.Exprs) == 1 {
					builder.WriteString(" OR ")
				} else {
					builder.WriteString(" AND ")
				}
			}

			if len(where.Exprs) > 1 {
				switch v := expr.(type) {
				case clause.OrConditions:
					if len(v.Exprs) == 1 {
						if e, ok := v.Exprs[0].(clause.Expr); ok {
							sql := strings.ToLower(e.SQL)
							wrapInParentheses = strings.Contains(sql, "and") || strings.Contains(sql, "or")
						}
					}
				case clause.AndConditions:
					if len(v.Exprs) == 1 {
						if e, ok := v.Exprs[0].(clause.Expr); ok {
							sql := strings.ToLower(e.SQL)
							wrapInParentheses = strings.Contains(sql, "and") || strings.Contains(sql, "or")
						}
					}
				case clause.Expr:
					sql := strings.ToLower(v.SQL)
					wrapInParentheses = strings.Contains(sql, "and") || strings.Contains(sql, "or")
				}
			}

			if wrapInParentheses {
				builder.WriteString(`(`)
				expr.Build(builder)
				builder.WriteString(`)`)
				wrapInParentheses = false
			} else {
				if e, ok := expr.(clause.IN); ok {
					if values, ok := e.Values[0].([]interface{}); ok {
						if len(values) > 1 {
							newExpr := clauses.IN{
								Column: expr.(clause.IN).Column,
								Values: expr.(clause.IN).Values,
							}
							newExpr.Build(builder)
							continue
						}
					}
				}

				expr.Build(builder)
			}
		}
	}
}

func (d Dialector) RewriteLimit(c clause.Clause, builder clause.Builder) {
	if limit, ok := c.Expression.(clause.Limit); ok {
		if stmt, ok := builder.(*gorm.Statement); ok {
			if _, ok := stmt.Clauses["ORDER BY"]; !ok {
				s := stmt.Schema
				builder.WriteString("ORDER BY ")
				if s != nil && s.PrioritizedPrimaryField != nil {
					builder.WriteQuoted(s.PrioritizedPrimaryField.DBName)
					builder.WriteByte(' ')
				} else {
					builder.WriteString("(SELECT NULL FROM ")
					builder.WriteString(d.DummyTableName())
					builder.WriteString(")")
				}
			}
		}

		if offset := limit.Offset; offset > 0 {
			builder.WriteString(" OFFSET ")
			builder.WriteString(strconv.Itoa(offset))
			builder.WriteString(" ROWS")
		}
		//兼容gorm1.25以前版本，新版本的limit是指针类型
		_limitInt, isInt := interface{}(limit.Limit).(int)
		if isInt && _limitInt > 0 {
			builder.WriteString(" FETCH NEXT ")
			builder.WriteString(strconv.Itoa(_limitInt))
			builder.WriteString(" ROWS ONLY")
		}

		_limitIntPtr, isIntPtr := interface{}(limit.Limit).(*int)
		if isIntPtr && *_limitIntPtr > 0 {
			builder.WriteString(" FETCH NEXT ")
			builder.WriteString(strconv.Itoa(*_limitIntPtr))
			builder.WriteString(" ROWS ONLY")
		}
	}
}

func (d Dialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{SQL: "VALUES (DEFAULT)"}
}

func (d Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{
		Migrator: migrator.Migrator{
			Config: migrator.Config{
				DB:                          db,
				Dialector:                   d,
				CreateIndexAfterCreateTable: true,
			},
		},
	}
}

func (d Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, v interface{}) {
	writer.WriteString(":")
	writer.WriteString(strconv.Itoa(len(stmt.Vars)))
}

func (d Dialector) QuoteTo(writer clause.Writer, str string) {
	writer.WriteString(str)
}

var numericPlaceholder = regexp.MustCompile(`:(\d+)`)

func (d Dialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, numericPlaceholder, `'`, funk.Map(vars, func(v interface{}) interface{} {
		switch v := v.(type) {
		case bool:
			if v {
				return 1
			}
			return 0
		default:
			return v
		}
	}).([]interface{})...)
}

func (d Dialector) DataTypeOf(field *schema.Field) string {
	if _, found := field.TagSettings["RESTRICT"]; found {
		delete(field.TagSettings, "RESTRICT")
	}

	var sqlType string

	addAutoIncrementWhenAvailable := func() {
		if field.AutoIncrement {
			sqlType += " GENERATED BY DEFAULT AS IDENTITY"
		}
	}

	switch field.DataType {
	case schema.Bool, schema.Int, schema.Uint, schema.Float:
		sqlType = "INTEGER"

		switch {
		case field.DataType == schema.Float:
			sqlType = "FLOAT"
		case field.Size <= 8:
			sqlType = "SMALLINT"
		}
		addAutoIncrementWhenAvailable()
	case schema.String:
		size := field.Size
		defaultSize := d.DefaultStringSize

		if size == 0 {
			if defaultSize > 0 {
				size = int(defaultSize)
			} else {
				hasIndex := field.TagSettings["INDEX"] != "" || field.TagSettings["UNIQUE"] != ""
				// TEXT, GEOMETRY or JSON column can't have a default value
				if field.PrimaryKey || field.HasDefaultValue || hasIndex {
					size = 191 // utf8mb4
				}
			}
		}

		if size >= 2000 {
			sqlType = "CLOB"
		} else {
			sqlType = fmt.Sprintf("VARCHAR2(%d)", size)
		}

	case schema.Time:
		sqlType = "TIMESTAMP"
		if field.NotNull || field.PrimaryKey {
			sqlType += " NOT NULL"
		}
	case schema.Bytes:
		sqlType = "BLOB"
	default:
		sqlType = string(field.DataType)

		if strings.EqualFold(sqlType, "text") {
			sqlType = "CLOB"
		}

		if sqlType == "" {
			panic(fmt.Sprintf("invalid sql type %s (%s) for oracle", field.FieldType.Name(), field.FieldType.String()))
		}

		notNull, _ := field.TagSettings["NOT NULL"]
		unique, _ := field.TagSettings["UNIQUE"]
		additionalType := fmt.Sprintf("%s %s", notNull, unique)
		if value, ok := field.TagSettings["DEFAULT"]; ok {
			additionalType = fmt.Sprintf("%s %s %s%s", "DEFAULT", value, additionalType, func() string {
				if value, ok := field.TagSettings["COMMENT"]; ok {
					return " COMMENT " + value
				}
				return ""
			}())
		}
		sqlType = fmt.Sprintf("%v %v", sqlType, additionalType)
	}

	return sqlType
}

func (d Dialector) SavePoint(tx *gorm.DB, name string) error {
	tx.Exec("SAVEPOINT " + name)
	return tx.Error
}

func (d Dialector) RollbackTo(tx *gorm.DB, name string) error {
	tx.Exec("ROLLBACK TO SAVEPOINT " + name)
	return tx.Error
}
