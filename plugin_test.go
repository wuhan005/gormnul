package gormnul

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lib/pq"
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

type collectionFixture struct {
	ID      int64 `gorm:"primaryKey;autoIncrement:false"`
	Payload datatypes.JSONMap
	Tags    pq.StringArray `gorm:"type:text[]"`
	Labels  pq.StringArray `gorm:"type:varchar(64)[]"`
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
	t.Cleanup(func() { _ = pool.Close() })

	if err := db.AutoMigrate(&releaseFixture{}, &serializedFixture{}, &defaultFixture{}, &hookFixture{}, &collectionFixture{}); err != nil {
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

	writeTests := []struct {
		name      string
		persisted bool
		write     func(row *releaseFixture) error
		wantNotes string
	}{
		{
			name:      "create_struct",
			write:     func(row *releaseFixture) error { return db.Create(row).Error },
			wantNotes: "修复问题",
		},
		{
			name: "create_map",
			write: func(row *releaseFixture) error {
				return db.Model(&releaseFixture{}).Create(map[string]any{
					"id":            row.ID,
					"release_notes": row.ReleaseNotes,
					"raw_json":      dirtyJSON,
					"nullable":      row.Nullable,
					"optional":      row.Optional,
					"binary":        row.Binary,
				}).Error
			},
			wantNotes: "修复问题",
		},
		{
			name: "skip_hooks",
			write: func(row *releaseFixture) error {
				return db.Session(&gorm.Session{SkipHooks: true}).Create(row).Error
			},
			wantNotes: "修复问题",
		},
		{
			name: "json_looking_text_untouched",
			write: func(row *releaseFixture) error {
				row.ReleaseNotes = dirtyJSON
				return db.Create(row).Error
			},
			wantNotes: dirtyJSON,
		},
		{
			name: "transaction_and_prepared_statements",
			write: func(row *releaseFixture) error {
				return db.Session(&gorm.Session{PrepareStmt: true}).Transaction(func(tx *gorm.DB) error {
					return tx.Create(row).Error
				})
			},
			wantNotes: "修复问题",
		},
		{
			name:      "save",
			persisted: true,
			write: func(row *releaseFixture) error {
				row.ReleaseNotes = "save\x00value"
				return db.Save(row).Error
			},
			wantNotes: "savevalue",
		},
		{
			name:      "updates_map",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Model(row).Updates(map[string]any{"release_notes": "map\x00value", "raw_json": dirtyJSON}).Error
			},
			wantNotes: "mapvalue",
		},
		{
			name:      "updates_struct_nul_only_becomes_empty",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Model(row).Updates(releaseFixture{ReleaseNotes: "\x00"}).Error
			},
			wantNotes: "",
		},
		{
			name:      "updates_struct_original_zero_is_omitted",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Model(row).Updates(releaseFixture{}).Error
			},
			wantNotes: "修复问题",
		},
		{
			name:      "update_column",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Model(row).UpdateColumn("release_notes", "column\x00value").Error
			},
			wantNotes: "columnvalue",
		},
		{
			name:      "update_columns_map",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Model(row).UpdateColumns(map[string]any{"release_notes": "columns\x00value", "raw_json": dirtyJSON}).Error
			},
			wantNotes: "columnsvalue",
		},
		{
			name:      "on_conflict_update_all",
			persisted: true,
			write: func(row *releaseFixture) error {
				row.ReleaseNotes = "upsert\x00value"
				return db.Clauses(clause.OnConflict{UpdateAll: true}).Create(row).Error
			},
			wantNotes: "upsertvalue",
		},
		{
			name:      "on_conflict_explicit_assignments",
			persisted: true,
			write: func(row *releaseFixture) error {
				return db.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "id"}},
					DoUpdates: clause.Assignments(map[string]any{"release_notes": "explicit\x00value", "raw_json": dirtyJSON}),
				}).Create(row).Error
			},
			wantNotes: "explicitvalue",
		},
	}
	for _, tc := range writeTests {
		t.Run(tc.name, func(t *testing.T) {
			row := newRow()
			if tc.persisted {
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := tc.write(&row); err != nil {
				t.Fatal(err)
			}
			check(t, row.ID, tc.wantNotes)
		})
	}

	t.Run("batch_pointer_slice", func(t *testing.T) {
		a, b := newRow(), newRow()
		if err := db.CreateInBatches([]*releaseFixture{&a, &b}, 1).Error; err != nil {
			t.Fatal(err)
		}
		check(t, a.ID, "修复问题")
		check(t, b.ID, "修复问题")
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

	rejectTests := []struct {
		name    string
		rawJSON string
		wantErr string
	}{
		{
			name:    "nul_key_rejected_before_execution",
			rawJSON: `{"a\u0000":1,"a":2}`,
			wantErr: "gormnul release_fixtures.raw_json: NUL in JSON key",
		},
		{
			name:    "invalid_json_rejected_before_execution",
			rawJSON: `{"bad":`,
			wantErr: "gormnul release_fixtures.raw_json: invalid JSON",
		},
	}
	for _, tc := range rejectTests {
		t.Run(tc.name, func(t *testing.T) {
			row := newRow()
			row.RawJSON = datatypes.JSON(tc.rawJSON)
			err := db.Create(&row).Error
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}

			var count int64
			if err := db.Model(&releaseFixture{}).Where("id = ?", row.ID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("rejected row was persisted")
			}
		})
	}

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

	t.Run("typed_json_table_map_without_model", func(t *testing.T) {
		values := map[string]any{"id": newID(), "raw_json": datatypes.JSON(dirtyJSON)}
		if err := db.Table("release_fixtures").Create(values).Error; err != nil {
			t.Fatal(err)
		}
	})

	checkCollection := func(t *testing.T, id int64, payload, tags, labels string) {
		t.Helper()
		var gotPayload, gotTags, gotLabels string
		query := `SELECT COALESCE(payload->>'s', ''), COALESCE(array_to_string(tags, ','), ''), COALESCE(array_to_string(labels, ','), '')
			FROM collection_fixtures WHERE id = ?`
		err := db.Raw(query, id).
			Row().
			Scan(&gotPayload, &gotTags, &gotLabels)
		if err != nil {
			t.Fatal(err)
		}
		if gotPayload != payload || gotTags != tags || gotLabels != labels {
			t.Fatalf("got payload=%q tags=%q labels=%q", gotPayload, gotTags, gotLabels)
		}
	}

	t.Run("json_map_and_text_arrays", func(t *testing.T) {
		row := collectionFixture{
			ID:      1,
			Payload: datatypes.JSONMap{"s": "a\x00b"},
			Tags:    pq.StringArray{"a\x00b", "c"},
			Labels:  pq.StringArray{"x\x00y"},
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		checkCollection(t, row.ID, "ab", "ab,c", "xy")

		if row.Labels[0] != "x\x00y" {
			t.Fatalf("input slice mutated: %q", row.Labels)
		}
	})

	t.Run("text_arrays_updates_map", func(t *testing.T) {
		row := collectionFixture{ID: 2}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}

		updates := map[string]any{"tags": pq.StringArray{"u\x00v"}, "labels": pq.StringArray{"m\x00n"}}
		if err := db.Model(&row).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		checkCollection(t, row.ID, "", "uv", "mn")
	})

	t.Run("json_map_table_map_without_model", func(t *testing.T) {
		values := map[string]any{"id": 3, "payload": datatypes.JSONMap{"s": "a\x00b"}}
		if err := db.Table("collection_fixtures").Create(values).Error; err != nil {
			t.Fatal(err)
		}
		checkCollection(t, 3, "ab", "", "")
	})
}
