package transcoder

import (
	"slices"
	"testing"
)

func TestHLSGOPArgsUseFourSecondClosedGOP(t *testing.T) {
	args := getHLSGOPArgs()
	wantPairs := [][2]string{
		{"-g", "999999"},
		{"-sc_threshold", "0"},
		{"-flags", "+cgop"},
		{"-force_key_frames", "expr:gte(t,n_forced*4)"},
	}
	for _, pair := range wantPairs {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == pair[0] && args[i+1] == pair[1] {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing GOP option %q %q in %v", pair[0], pair[1], args)
		}
	}
}

func TestEncoderArgsDisableAdaptiveKeyframes(t *testing.T) {
	tests := []struct {
		encoder string
		want    []string
	}{
		{EncoderCPU, []string{"-x264-params", "scenecut=0:open-gop=0"}},
		{EncoderNVENC, []string{"-no-scenecut", "1", "-forced-idr", "1"}},
		{EncoderAMF, []string{"-forced_idr", "1"}},
		{EncoderQSV, []string{"-forced_idr", "1"}},
	}
	for _, tt := range tests {
		args := getEncoderArgs(tt.encoder, 720, 0, 0)
		for i := 0; i < len(tt.want); i += 2 {
			pair := []string{tt.want[i], tt.want[i+1]}
			found := false
			for j := 0; j+1 < len(args); j++ {
				if slices.Equal(args[j:j+2], pair) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s missing option %v in %v", tt.encoder, pair, args)
			}
		}
	}
}
