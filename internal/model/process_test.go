package model

import (
	"reflect"
	"testing"
)

func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"env PORT=12390 /home/yury/run.sh", []string{"env", "PORT=12390", "/home/yury/run.sh"}},
		{`cmd /c "set PORT=12390 & llama-server.exe --port 12390"`, []string{"cmd", "/c", "set PORT=12390 & llama-server.exe --port 12390"}},
		{`cmd /c echo hello`, []string{"cmd", "/c", "echo", "hello"}},
		{`"/usr/bin/server" --port 1`, []string{"/usr/bin/server", "--port", "1"}},
		{"", nil},
	}
	for _, tc := range cases {
		got, err := splitCommandLine(tc.in)
		if err != nil {
			t.Fatalf("splitCommandLine(%q): %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCommandLine(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	if _, err := splitCommandLine(`cmd /c "unbalanced`); err == nil {
		t.Fatal("expected error for unbalanced quote")
	}
}
