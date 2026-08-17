package models

import "testing"

func TestMediaObjectPathUsesPersistedPath(t *testing.T) {
	fileID, sourceID := "clone-file", "source-file"
	objectPath := "legacy-owner/file_original.mp4"
	media := Media{FileID: &fileID, ClonedFrom: &sourceID, Path: &objectPath}
	if got := media.ObjectPath(); got != objectPath {
		t.Fatalf("ObjectPath() = %q, want %q", got, objectPath)
	}
}

func TestMediaObjectPathUsesCloneOwnerForLegacyMedia(t *testing.T) {
	fileID, sourceID, fileName := "clone-file", "source-file", "file_original.mp4"
	media := Media{FileID: &fileID, ClonedFrom: &sourceID, FileName: &fileName}
	if got, want := media.ObjectPath(), "source-file/file_original.mp4"; got != want {
		t.Fatalf("ObjectPath() = %q, want %q", got, want)
	}
}
