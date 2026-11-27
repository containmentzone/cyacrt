package main

import (
	"slices"
	"testing"
)

func TestParseFilesystems(t *testing.T) {
	in := "nodev\tsysfs\nnodev\ttmpfs\n\text3\n\text2\n\text4\nnodev\tproc\n\txfs\n"
	want := []string{"ext4", "ext3", "ext2", "xfs"}
	if got := parseFilesystems(in); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
