package config

import "testing"

func TestGetMediaLayoutEnv(t *testing.T) {
	t.Run("defaults to muxed", func(t *testing.T) {
		t.Setenv("MEDIA_LAYOUT", "")
		if got := getMediaLayoutEnv(); got != "muxed" {
			t.Fatalf("got %q, want muxed", got)
		}
	})
	t.Run("accepts separated", func(t *testing.T) {
		t.Setenv("MEDIA_LAYOUT", "separated")
		if got := getMediaLayoutEnv(); got != "separated" {
			t.Fatalf("got %q, want separated", got)
		}
	})
	t.Run("invalid falls back to muxed", func(t *testing.T) {
		t.Setenv("MEDIA_LAYOUT", "invalid")
		if got := getMediaLayoutEnv(); got != "muxed" {
			t.Fatalf("got %q, want muxed", got)
		}
	})
}

func TestGetTranscodePipelineModeEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "defaults to adaptive", want: "adaptive"},
		{name: "accepts adaptive", value: "adaptive", want: "adaptive"},
		{name: "accepts fanout", value: "fanout", want: "fanout"},
		{name: "accepts sequential", value: "sequential", want: "sequential"},
		{name: "invalid falls back to adaptive", value: "invalid", want: "adaptive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TRANSCODE_PIPELINE_MODE", tt.value)
			if got := getTranscodePipelineModeEnv(); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetBoolEnv(t *testing.T) {
	t.Setenv("TEST_BOOL", "true")
	if !getBoolEnv("TEST_BOOL", false) {
		t.Fatal("true value was not accepted")
	}
	t.Setenv("TEST_BOOL", "false")
	if getBoolEnv("TEST_BOOL", true) {
		t.Fatal("false value was not accepted")
	}
	t.Setenv("TEST_BOOL", "invalid")
	if !getBoolEnv("TEST_BOOL", true) {
		t.Fatal("invalid value did not use fallback")
	}
}
