package gormnul

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type releaseFixture struct {
	ID           int64 `gorm:"primaryKey;autoIncrement:false"`
	ReleaseNotes string
	RawJSON      datatypes.JSON `gorm:"type:jsonb"`
	Nullable     *string
	Optional     sql.NullString
	Binary       []byte `gorm:"type:bytea"`
}

type serializedFixture struct {
	ID      int64          `gorm:"primaryKey;autoIncrement:false"`
	Payload map[string]any `gorm:"type:jsonb;serializer:json"`
}

type defaultFixture struct {
	ID    int64  `gorm:"primaryKey;autoIncrement:false"`
	Label string `gorm:"default:defaultlabel"`
}

type hookFixture struct {
	ID      int64 `gorm:"primaryKey;autoIncrement:false"`
	Notes   string
	Payload datatypes.JSON `gorm:"type:jsonb"`
}

func (h *hookFixture) BeforeSave(_ *gorm.DB) error {
	h.Notes = "hook\x00value"
	h.Payload = datatypes.JSON(`{"s":"hook\u0000value"}`)
	return nil
}

const dirtyJSON = `{"s":"修复已知问题\u0000","literal":"\\u0000","id":7653202734459387904,"emoji":"😀","array":["a\u0000b",null,true]}`

func TestGORMPostgres(t *testing.T) {
	dsn := os.Getenv("PG_NUL_DSN")
	if dsn == "" {
		t.Skip("PG_NUL_DSN must identify an isolated test database")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := db.AutoMigrate(&releaseFixture{}, &serializedFixture{}, &defaultFixture{}, &hookFixture{}); err != nil {
		t.Fatal(err)
	}

	t.Run("baseline_reproduces_exact_error", func(t *testing.T) {
		row := releaseFixture{ID: -1, RawJSON: datatypes.JSON(dirtyJSON)}
		err := db.Create(&row).Error
		if err == nil || !strings.Contains(err.Error(), "unsupported Unicode escape sequence") {
			t.Fatalf("expected original error, got %v", err)
		}
	})

	if err := db.Use(Plugin{}); err != nil {
		t.Fatal(err)
	}

	var nextID int64
	newID := func() int64 {
		nextID++
		return nextID
	}
	newRow := func() releaseFixture {
		note := "nullable\x00text"
		return releaseFixture{
			ID:           newID(),
			ReleaseNotes: "修复\x00问题",
			RawJSON:      datatypes.JSON(dirtyJSON),
			Nullable:     &note,
			Optional:     sql.NullString{String: "optional\x00text", Valid: true},
			Binary:       []byte{'a', 0, 'b'},
		}
	}
	create := func(t *testing.T) releaseFixture {
		t.Helper()
		row := newRow()
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	check := func(t *testing.T, id int64, note string) {
		t.Helper()
		var row releaseFixture
		if err := db.First(&row, id).Error; err != nil {
			t.Fatal(err)
		}
		if row.ReleaseNotes != note {
			t.Fatalf("notes: got %q, want %q", row.ReleaseNotes, note)
		}

		var data map[string]json.RawMessage
		if err := json.Unmarshal(row.RawJSON, &data); err != nil {
			t.Fatal(err)
		}
		var s, literal, emoji string
		for key, dst := range map[string]*string{"s": &s, "literal": &literal, "emoji": &emoji} {
			if err := json.Unmarshal(data[key], dst); err != nil {
				t.Fatal(err)
			}
		}
		if s != "修复已知问题" || literal != `\u0000` || emoji != "😀" || string(data["id"]) != "7653202734459387904" {
			t.Fatalf("JSON changed unexpectedly: %s", row.RawJSON)
		}
		if string(data["array"]) != `["ab", null, true]` {
			t.Fatalf("nested JSON not cleaned: %s", data["array"])
		}

		if !bytes.Equal(row.Binary, []byte{'a', 0, 'b'}) {
			t.Fatalf("binary changed: %v", row.Binary)
		}
		if row.Nullable == nil || *row.Nullable != "nullabletext" || !row.Optional.Valid || row.Optional.String != "optionaltext" {
			t.Fatalf("nullable text not normalized: %#v", row)
		}
	}

	t.Run("create_struct", func(t *testing.T) {
		row := create(t)
		check(t, row.ID, "修复问题")
	})

	t.Run("batch_pointer_slice", func(t *testing.T) {
		a, b := newRow(), newRow()
		if err := db.CreateInBatches([]*releaseFixture{&a, &b}, 1).Error; err != nil {
			t.Fatal(err)
		}
		check(t, a.ID, "修复问题")
		check(t, b.ID, "修复问题")
	})

	t.Run("create_map", func(t *testing.T) {
		row := newRow()
		values := map[string]any{
			"id":            row.ID,
			"release_notes": row.ReleaseNotes,
			"raw_json":      dirtyJSON,
			"nullable":      row.Nullable,
			"optional":      row.Optional,
			"binary":        row.Binary,
		}
		if err := db.Model(&releaseFixture{}).Create(values).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "修复问题")
	})

	t.Run("create_batch_map", func(t *testing.T) {
		a, b := newRow(), newRow()
		rows := []map[string]any{
			{"id": a.ID, "release_notes": "a\x00b", "raw_json": dirtyJSON},
			{"id": b.ID, "release_notes": "a\x00b", "raw_json": dirtyJSON},
		}
		if err := db.Model(&releaseFixture{}).Create(rows).Error; err != nil {
			t.Fatal(err)
		}

		var got releaseFixture
		if err := db.First(&got, a.ID).Error; err != nil || got.ReleaseNotes != "ab" {
			t.Fatalf("%#v; %v", got, err)
		}
	})

	t.Run("save", func(t *testing.T) {
		row := create(t)
		row.ReleaseNotes = "save\x00value"
		if err := db.Save(&row).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "savevalue")
	})

	t.Run("updates_map", func(t *testing.T) {
		row := create(t)
		updates := map[string]any{"release_notes": "map\x00value", "raw_json": dirtyJSON}
		if err := db.Model(&row).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "mapvalue")
	})

	t.Run("updates_struct_nul_only_becomes_empty", func(t *testing.T) {
		row := create(t)
		if err := db.Model(&row).Updates(releaseFixture{ReleaseNotes: "\x00"}).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "")
	})

	t.Run("updates_struct_original_zero_is_omitted", func(t *testing.T) {
		row := create(t)
		if err := db.Model(&row).Updates(releaseFixture{}).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "修复问题")
	})

	t.Run("update_column", func(t *testing.T) {
		row := create(t)
		if err := db.Model(&row).UpdateColumn("release_notes", "column\x00value").Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "columnvalue")
	})

	t.Run("update_columns_map", func(t *testing.T) {
		row := create(t)
		updates := map[string]any{"release_notes": "columns\x00value", "raw_json": dirtyJSON}
		if err := db.Model(&row).UpdateColumns(updates).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "columnsvalue")
	})

	t.Run("skip_hooks", func(t *testing.T) {
		row := newRow()
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "修复问题")
	})

	t.Run("on_conflict_update_all", func(t *testing.T) {
		row := create(t)
		row.ReleaseNotes = "upsert\x00value"
		row.RawJSON = datatypes.JSON(dirtyJSON)
		if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "upsertvalue")
	})

	t.Run("on_conflict_explicit_assignments", func(t *testing.T) {
		row := create(t)
		onConflict := clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{"release_notes": "explicit\x00value", "raw_json": dirtyJSON}),
		}
		if err := db.Clauses(onConflict).Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "explicitvalue")
	})

	t.Run("where_parameters_untouched", func(t *testing.T) {
		stmt := db.Session(&gorm.Session{DryRun: true}).
			Model(&releaseFixture{}).
			Where("release_notes = ?", "match\x00original").
			Update("release_notes", "write\x00clean").
			Statement
		if stmt.Error != nil {
			t.Fatal(stmt.Error)
		}
		if len(stmt.Vars) != 2 || stmt.Vars[0] != "writeclean" || stmt.Vars[1] != "match\x00original" {
			t.Fatalf("parameters changed incorrectly: %#v", stmt.Vars)
		}
	})

	t.Run("json_looking_text_untouched", func(t *testing.T) {
		row := newRow()
		row.ReleaseNotes = dirtyJSON
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, dirtyJSON)
	})

	t.Run("sql_null_and_json_null_distinct", func(t *testing.T) {
		a := releaseFixture{ID: newID()}
		b := releaseFixture{ID: newID(), RawJSON: datatypes.JSON("null")}
		if err := db.Create(&[]releaseFixture{a, b}).Error; err != nil {
			t.Fatal(err)
		}

		isNull := func(id int64) bool {
			t.Helper()
			var null bool
			if err := db.Raw("SELECT raw_json IS NULL FROM release_fixtures WHERE id = ?", id).Scan(&null).Error; err != nil {
				t.Fatal(err)
			}
			return null
		}
		if sqlNull, jsonNull := isNull(a.ID), isNull(b.ID); !sqlNull || jsonNull {
			t.Fatalf("SQL null=%v, JSON null=%v", sqlNull, jsonNull)
		}
	})

	t.Run("nul_key_rejected_before_execution", func(t *testing.T) {
		row := newRow()
		row.RawJSON = datatypes.JSON(`{"a\u0000":1,"a":2}`)
		err := db.Create(&row).Error
		if err == nil || !strings.Contains(err.Error(), "gormnul release_fixtures.raw_json: NUL in JSON key") {
			t.Fatalf("expected local key error, got %v", err)
		}

		var count int64
		if err := db.Model(&releaseFixture{}).Where("id = ?", row.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("rejected row was persisted")
		}
	})

	t.Run("invalid_json_rejected_before_execution", func(t *testing.T) {
		row := newRow()
		row.RawJSON = datatypes.JSON(`{"bad":`)
		err := db.Create(&row).Error
		if err == nil || !strings.Contains(err.Error(), "gormnul") {
			t.Fatalf("expected local validation error, got %v", err)
		}
	})

	t.Run("omitted_invalid_field_is_not_processed", func(t *testing.T) {
		row := newRow()
		row.RawJSON = datatypes.JSON(`{"bad":`)
		if err := db.Omit("raw_json").Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("text_default_not_substituted_for_nul_only", func(t *testing.T) {
		row := defaultFixture{ID: 1, Label: "\x00"}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}

		var got defaultFixture
		if err := db.First(&got, 1).Error; err != nil || got.Label != "" {
			t.Fatalf("default incorrectly applied: %#v; %v", got, err)
		}
	})

	t.Run("serializer_json", func(t *testing.T) {
		row := serializedFixture{ID: 1, Payload: map[string]any{"s": "a\x00b", "id": json.Number("7653202734459387904")}}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}

		var s string
		if err := db.Raw("SELECT payload->>'s' FROM serialized_fixtures WHERE id = 1").Scan(&s).Error; err != nil || s != "ab" {
			t.Fatalf("serializer result %q; %v", s, err)
		}
	})

	t.Run("before_save_generated_data", func(t *testing.T) {
		row := hookFixture{ID: 1}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}

		var got hookFixture
		if err := db.First(&got, 1).Error; err != nil || got.Notes != "hookvalue" {
			t.Fatalf("hook value %#v; %v", got, err)
		}

		var payload map[string]string
		if err := json.Unmarshal(got.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["s"] != "hookvalue" {
			t.Fatalf("hook JSON %s", got.Payload)
		}
	})

	t.Run("transaction_and_prepared_statements", func(t *testing.T) {
		row := newRow()
		err := db.Session(&gorm.Session{PrepareStmt: true}).Transaction(func(tx *gorm.DB) error {
			return tx.Create(&row).Error
		})
		if err != nil {
			t.Fatal(err)
		}
		check(t, row.ID, "修复问题")
	})

	t.Run("typed_json_table_map_without_model", func(t *testing.T) {
		values := map[string]any{"id": newID(), "raw_json": datatypes.JSON(dirtyJSON)}
		if err := db.Table("release_fixtures").Create(values).Error; err != nil {
			t.Fatal(err)
		}
	})
}
