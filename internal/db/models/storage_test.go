package models

import "testing"

func TestGetOriginObjectURL(t *testing.T) {
	origin := "origin.example.com/base"
	storage := Storage{Type: "s3", OriginURL: &origin}

	got, err := storage.GetOriginObjectURL("file-id", "file_original.mp4")
	if err != nil {
		t.Fatalf("GetOriginObjectURL() error = %v", err)
	}
	if want := "https://origin.example.com/base/file-id/file_original.mp4"; got != want {
		t.Fatalf("GetOriginObjectURL() = %q, want %q", got, want)
	}
}

func TestGetOriginObjectURLRejectsQuery(t *testing.T) {
	origin := "https://origin.example.com?token=secret"
	storage := Storage{Type: "s3", OriginURL: &origin}
	if _, err := storage.GetOriginObjectURL("file-id", "file_original.mp4"); err == nil {
		t.Fatal("expected originUrl query to be rejected")
	}
}

func TestGetOriginObjectPathURLPreservesCloneOwner(t *testing.T) {
	origin := "https://origin.example.com/base"
	storage := Storage{Type: "s3", OriginURL: &origin}
	got, err := storage.GetOriginObjectPathURL("source-file/file_original.mp4")
	if err != nil {
		t.Fatalf("GetOriginObjectPathURL() error = %v", err)
	}
	if want := "https://origin.example.com/base/source-file/file_original.mp4"; got != want {
		t.Fatalf("GetOriginObjectPathURL() = %q, want %q", got, want)
	}
}
