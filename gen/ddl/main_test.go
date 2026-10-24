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

func TestIdentityOptions(t *testing.T) {
	cases := []struct {
		start   int64
		step    int64
		want    string
		wantErr bool
	}{
		{0, 0, "", false},
		{1000, 0, " (START WITH 1000)", false},
		{0, 10, " (INCREMENT BY 10)", false},
		{1000, 10, " (START WITH 1000 INCREMENT BY 10)", false},
		{1000, 1, " (START WITH 1000)", false},
		{0, 1, "", false},
		{0, -1, "", true},
	}

	for _, c := range cases {
		got, err := identityOptions(c.start, c.step)
		if c.wantErr {
			if err == nil {
				t.Fatalf("start=%d step=%d should return an error", c.start, c.step)
			}
			continue
		}
		if err != nil {
			t.Fatalf("start=%d step=%d failed: %v", c.start, c.step, err)
		}
		if got != c.want {
			t.Fatalf("start=%d step=%d = %q, want %q", c.start, c.step, got, c.want)
		}
	}
}
