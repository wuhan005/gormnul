// Package gormnul removes NUL from PostgreSQL text and JSON write values.
// Register Plugin once, before the root GORM DB is used concurrently.
// It changes bound write values, not the input model or WHERE conditions.
// Model schema must identify column types. Table-only maps with untyped JSON
// strings, raw SQL, and SQL expressions need a separate typed write boundary.
package gormnul

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Plugin applies an intentionally lossy policy: delete NUL in text/JSON values,
// reject JSON keys containing NUL, and preserve binary data. It handles GORM's
// VALUES, SET and ON CONFLICT clauses. Raw SQL and SQL expressions are opaque.
type Plugin struct{}

func (Plugin) Name() string { return "postgres:nul_write_values" }

func (Plugin) Initialize(db *gorm.DB) error {
	if db.Name() != "postgres" {
		return errors.New("gormnul requires the postgres dialect")
	}

	if db.ClauseBuilders == nil {
		db.ClauseBuilders = make(map[string]clause.ClauseBuilder)
	}
	for _, name := range []string{"VALUES", "SET", "ON CONFLICT"} {
		db.ClauseBuilders[name] = wrapClauseBuilder(db.ClauseBuilders[name])
	}
	return nil
}

// wrapClauseBuilder cleans write values in the clause, then delegates to the
// previous builder, or to GORM's default build when there is none.
func wrapClauseBuilder(previous clause.ClauseBuilder) clause.ClauseBuilder {
	return func(c clause.Clause, builder clause.Builder) {
		if stmt, ok := builder.(*gorm.Statement); ok {
			clean, err := normalizeClause(stmt, c.Expression)
			if err != nil {
				_ = builder.AddError(err)
				return
			}
			c.Expression = clean
		}

		if previous != nil {
			previous(c, builder)
			return
		}
		c.Build(builder)
	}
}

func normalizeClause(stmt *gorm.Statement, expression clause.Expression) (clause.Expression, error) {
	switch value := expression.(type) {
	case clause.Values:
		return normalizeValues(stmt, value)
	case clause.Set:
		return normalizeSet(stmt, value)
	case clause.OnConflict:
		updates, err := normalizeSet(stmt, value.DoUpdates)
		if err != nil {
			return nil, err
		}
		value.DoUpdates = updates
		return value, nil
	default:
		return expression, nil
	}
}

func normalizeValues(stmt *gorm.Statement, values clause.Values) (clause.Values, error) {
	rows := make([][]any, len(values.Values))
	for i, row := range values.Values {
		if len(row) > len(values.Columns) {
			return clause.Values{}, errors.New("gormnul: VALUES column count mismatch")
		}

		rows[i] = make([]any, len(row))
		for j, item := range row {
			clean, err := normalizeColumn(stmt, values.Columns[j].Name, item)
			if err != nil {
				return clause.Values{}, err
			}
			rows[i][j] = clean
		}
	}

	values.Values = rows
	return values, nil
}

func normalizeSet(stmt *gorm.Statement, set clause.Set) (clause.Set, error) {
	result := slices.Clone(set)
	for i, assignment := range set {
		clean, err := normalizeColumn(stmt, assignment.Column.Name, assignment.Value)
		if err != nil {
			return nil, err
		}
		result[i].Value = clean
	}
	return result, nil
}

func normalizeColumn(stmt *gorm.Statement, column string, value any) (any, error) {
	if isNil(value) {
		return value, nil
	}

	// An SQL expression can have unrelated argument types, so do not infer them
	// from the destination column. Raw SQL has the same limitation.
	switch value.(type) {
	case clause.Expression, clause.Column, clause.Table:
		return value, nil
	}

	var field *schema.Field
	if stmt.Schema != nil {
		field = stmt.Schema.LookUpField(column)
	}

	normalize := normalizerFor(field, value)
	if normalize == nil {
		return value, nil
	}

	clean, err := normalize(value)
	if err != nil {
		return nil, fmt.Errorf("gormnul %s.%s: %w", stmt.Table, column, err)
	}
	return clean, nil
}

// normalizerFor picks the cleaner for the column type, preferring JSON over
// text. It returns nil when the value needs no cleaning.
func normalizerFor(field *schema.Field, value any) func(any) (any, error) {
	if field == nil {
		// Typed JSON values remain identifiable in map writes without a model.
		if typed, ok := value.(interface{ GormDataType() string }); ok && isJSONType(typed.GormDataType()) {
			return normalizeJSON
		}
		return nil
	}

	dataType, gormDataType := string(field.DataType), string(field.GORMDataType)
	switch {
	case field.TagSettings["SERIALIZER"] == "json", isJSONType(dataType), isJSONType(gormDataType):
		return normalizeJSON
	case isTextType(dataType), isTextType(gormDataType), isTextArrayType(dataType):
		return normalizeText
	default:
		return nil
	}
}

func isJSONType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "json", "jsonb", "jsonmap":
		return true
	}
	return false
}

func isTextType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "string", "text":
		return true
	}
	return false
}

// isTextArrayType reports whether dataType is a one-dimensional text array,
// such as text[] or varchar(64)[].
func isTextArrayType(dataType string) bool {
	elem, ok := strings.CutSuffix(strings.ToLower(strings.TrimSpace(dataType)), "[]")
	if !ok {
		return false
	}
	if i := strings.IndexByte(elem, '('); i >= 0 {
		elem = elem[:i]
	}

	switch strings.TrimSpace(elem) {
	case "text", "varchar", "character varying", "char", "character", "bpchar", "citext":
		return true
	}
	return false
}

func isNil(value any) bool {
	if value == nil {
		return true
	}

	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func resolveValuer(value any) (any, error) {
	if valuer, ok := value.(driver.Valuer); ok {
		return valuer.Value()
	}
	return value, nil
}

func normalizeText(value any) (any, error) {
	// Context-dependent SQL-valuers are opaque, just like gorm.Expr.
	if _, ok := value.(gorm.Valuer); ok {
		return value, nil
	}

	value, err := resolveValuer(value)
	if err != nil || value == nil {
		return nil, err
	}

	if raw, ok := value.([]byte); ok {
		return bytes.ReplaceAll(raw, []byte{0}, nil), nil
	}

	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.String || !strings.ContainsRune(v.String(), 0) {
		return value, nil
	}

	clean := reflect.New(v.Type()).Elem()
	clean.SetString(strings.ReplaceAll(v.String(), "\x00", ""))
	return clean.Interface(), nil
}

func normalizeJSON(value any) (any, error) {
	value, err := resolveValuer(value)
	if err != nil || value == nil {
		return nil, err
	}

	var raw []byte
	switch v := value.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	case json.RawMessage:
		raw = v
	default:
		if raw, err = json.Marshal(value); err != nil {
			return nil, err
		}
	}

	clean, err := sanitizeJSON(raw)
	if err != nil {
		return nil, err
	}
	return string(clean), nil
}

func sanitizeJSON(raw []byte) ([]byte, error) {
	// The decoder reads only the first JSON value, so validate the whole input
	// first to reject trailing data.
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}

	// Valid JSON can only carry NUL as a \u0000 escape. Inputs without one are
	// returned as is, keeping their exact text and skipping the costly rewrite.
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw, nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}

	clean, err := cleanJSONValue(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(clean)
}

func cleanJSONValue(value any) (any, error) {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", ""), nil
	case []any:
		for i, item := range v {
			clean, err := cleanJSONValue(item)
			if err != nil {
				return nil, err
			}
			v[i] = clean
		}
	case map[string]any:
		for key, item := range v {
			if strings.ContainsRune(key, 0) {
				return nil, fmt.Errorf("NUL in JSON key %q", key)
			}

			clean, err := cleanJSONValue(item)
			if err != nil {
				return nil, err
			}
			v[key] = clean
		}
	}
	return value, nil
}
