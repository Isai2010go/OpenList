package blomp

import "testing"

func TestSplitStoragePath(t *testing.T) {
	tests := []struct {
		path, container, object string
		ok bool
	}{
		{"", "", "", false},
		{"/", "", "", false},
		{"bucket", "bucket", "", true},
		{"/bucket/", "bucket", "", true},
		{"bucket/folder/file.txt", "bucket", "folder/file.txt", true},
		{ "bucket/folder with spaces/a.txt", "bucket", "folder with spaces/a.txt", true },
	}
	for _, tt := range tests {
		container, object, ok := splitStoragePath(tt.path)
		if container != tt.container || object != tt.object || ok != tt.ok {
			t.Errorf("splitStoragePath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.path, container, object, ok, tt.container, tt.object, tt.ok)
		}
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.txt", true},
		{"folder name", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{ "a\\\\b", false },
	}
	for _, tt := range tests {
		if got := validName(tt.name); got != tt.want {
			t.Errorf("validName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
