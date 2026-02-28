package main

import (
"testing"
)

func TestGetDisks(t *testing.T) {
	disks, err := getAvailableDisks()
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range disks {
		t.Logf("Disk %d: Path=%q Model=%q Serial=%q Cap=%.2f\n", i, d.Path, d.Model, d.Serial, d.CapacityGB)
	}
}
