package server

import "testing"

func TestAutoChunkSize(t *testing.T) {
	const MiB = 1 << 20
	cases := []struct {
		name string
		size int64
		want int64
	}{
		{"tiny file floors at 5MiB", 1024, 5 * MiB},
		{"5MiB exactly", 5 * MiB, 5 * MiB},
		{"under 64 parts keeps 5MiB", 320 * MiB, 5 * MiB},
		{"1GiB divides into 16MiB", 1 << 30, 16 * MiB},
		{"just over 1GiB rounds up to whole MiB", 1<<30 + 1, 17 * MiB},
		{"huge file caps at 95MiB", 10 << 30, 95 * MiB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoChunkSize(tc.size); got != tc.want {
				t.Fatalf("autoChunkSize(%d) = %d, want %d", tc.size, got, tc.want)
			}
		})
	}

	// Every size must yield a whole-MiB chunk within [5MiB, 95MiB].
	for _, size := range []int64{1, 3 * MiB, 999 * MiB, 5 << 30, 100 << 30} {
		c := autoChunkSize(size)
		if c < 5*MiB || c > 95*MiB || c%MiB != 0 {
			t.Errorf("autoChunkSize(%d) = %d, out of bounds or not MiB-aligned", size, c)
		}
	}
}
