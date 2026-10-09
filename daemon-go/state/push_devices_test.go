package state

import (
	"context"
	"testing"
)

func TestPushDeviceLifecycle(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()
	if err := s.UpsertPushDevice(ctx, PushDevice{Token: "a", Environment: "sandbox", Name: "phone"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPushDevice(ctx, PushDevice{Token: "a", Environment: "production", Name: "phone"}); err != nil {
		t.Fatal(err)
	}
	devices, err := s.ListPushDevices(ctx)
	if err != nil || len(devices) != 1 || devices[0].Environment != "production" {
		t.Fatalf("devices = %+v, err = %v", devices, err)
	}
	removed, err := s.DeletePushDevices(ctx, "a", "missing")
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d, err = %v", removed, err)
	}
	if devices, _ := s.ListPushDevices(ctx); len(devices) != 0 {
		t.Fatalf("devices after delete = %+v", devices)
	}
}
