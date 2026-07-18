package controller

import "testing"

func TestEqualStringSets(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		// The drift-loop bug: the Admin API returns empty array columns as a
		// non-nil empty slice, while an unset CR field is nil. reflect.DeepEqual
		// reports these as different, causing perpetual false drift.
		{"nil vs empty", nil, []string{}, true},
		{"empty vs nil", []string{}, nil, true},
		{"both nil", nil, nil, true},
		{"same single", []string{"a"}, []string{"a"}, true},
		{"same, different order", []string{"a", "b"}, []string{"b", "a"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different element", []string{"a"}, []string{"b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := equalStringSets(tc.a, tc.b); got != tc.want {
				t.Errorf("equalStringSets(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
