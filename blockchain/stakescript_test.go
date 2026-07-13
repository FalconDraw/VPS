package blockchain

import "testing"

func TestIsDrawBlock(t *testing.T) {
	cases := []struct {
		height int32
		want   bool
	}{
		{0, false},
		{1, false},
		{143, false},
		{144, true},
		{145, false},
		{288, true},
		{-1, false},
	}
	for _, tc := range cases {
		if got := IsDrawBlock(tc.height); got != tc.want {
			t.Errorf("IsDrawBlock(%d) = %v, want %v", tc.height, got, tc.want)
		}
	}
}

func TestNextDrawHeight(t *testing.T) {
	cases := []struct {
		current int32
		want    int32
	}{
		{0, 36},
		{1, 36},
		{35, 36},
		{36, 72},
		{37, 72},
		{71, 72},
		{72, 108},
	}
	for _, tc := range cases {
		if got := NextDrawHeight(tc.current); got != tc.want {
			t.Errorf("NextDrawHeight(%d) = %d, want %d", tc.current, got, tc.want)
		}
	}
}
