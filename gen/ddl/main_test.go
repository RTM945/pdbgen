package main

import "testing"

func TestSQLType(t *testing.T) {
	cases := []struct {
		goType     string
		columnType string
		defaultVal string
	}{
		{"int64", "BIGINT", "0"},
		{"int32", "INTEGER", "0"},
		{"int16", "SMALLINT", "0"},
		{"int8", "SMALLINT", "0"},
		{"string", "TEXT", "''"},
		{"bool", "BOOLEAN", "false"},
		{"float32", "REAL", "0"},
		{"float64", "DOUBLE PRECISION", "0"},
	}

	for _, c := range cases {
		columnType, defaultVal, err := sqlType(c.goType)
		if err != nil {
			t.Fatalf("sqlType(%q) failed: %v", c.goType, err)
		}
		if columnType != c.columnType || defaultVal != c.defaultVal {
			t.Fatalf(
				"sqlType(%q) = (%q, %q), want (%q, %q)",
				c.goType, columnType, defaultVal, c.columnType, c.defaultVal,
			)
		}
	}

	if _, _, err := sqlType("decimal"); err == nil {
		t.Fatal("unsupported type should return an error")
	}
}
