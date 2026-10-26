package cmd

import (
	"strings"
	"testing"
)

func TestAnalyzeFlags_DefinedOnMigrateAndRewrite(t *testing.T) {
	cases := map[string]func(string) bool{
		"migrate": func(f string) bool { return migrateCmd.Flags().Lookup(f) != nil },
		"rewrite": func(f string) bool { return rewriteCmd.Flags().Lookup(f) != nil },
	}
	for name, has := range cases {
		for _, flag := range []string{"analyze", "strip-large-files"} {
			if !has(flag) {
				t.Errorf("%s: missing --%s flag", name, flag)
			}
		}
	}

	for _, f := range []string{"analyze", "strip-large-files"} {
		if migrateCmd.Flags().Lookup(f).Usage != rewriteCmd.Flags().Lookup(f).Usage {
			t.Errorf("--%s help text differs between migrate and rewrite", f)
		}
	}
	if !strings.Contains(rewriteCmd.Long, "--analyze") {
		t.Error("rewrite Long help should document --analyze")
	}
}

func TestValidateAnalyzeLock(t *testing.T) {
	tests := []struct {
		analyze, strip, lock bool
		wantErr              bool
	}{
		{analyze: true, strip: false, lock: true, wantErr: true},
		{analyze: true, strip: false, lock: false},
		{analyze: true, strip: true, lock: true},
		{analyze: false, strip: false, lock: true},
		{analyze: false, strip: true, lock: true},
	}
	for _, tt := range tests {
		err := validateAnalyzeLock(tt.analyze, tt.strip, tt.lock)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateAnalyzeLock(analyze=%v, strip=%v, lock=%v) err=%v, wantErr=%v",
				tt.analyze, tt.strip, tt.lock, err, tt.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "--lock-repositories") {
			t.Errorf("error should mention --lock-repositories: %v", err)
		}
	}
}
