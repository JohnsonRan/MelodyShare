package config

import "testing"

func TestLoadChunkSize(t *testing.T) {
	cases := []struct {
		env     string
		want    int64
		wantErr bool
	}{
		{"", 0, false}, // default is auto
		{"auto", 0, false},
		{"0", 0, false},
		{"64", 64 << 20, false},
		{"5", 5 << 20, false},
		{"95", 95 << 20, false},
		{"3", 0, true},
		{"96", 0, true},
		{"xyz", 0, true},
	}
	for _, tc := range cases {
		t.Run("SHARE_CHUNK_SIZE_MB="+tc.env, func(t *testing.T) {
			t.Setenv("SHARE_CHUNK_SIZE_MB", tc.env)
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.env)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ChunkSize != tc.want {
				t.Fatalf("ChunkSize = %d, want %d", cfg.ChunkSize, tc.want)
			}
		})
	}
}
