package main

import "testing"

func TestBuildFilenameIncludesAvailableMetadata(t *testing.T) {
	got := buildFilename(16, "ttdceq", ".webm", routine{
		name:      "Alyssa Rantino",
		apparatus: "Floor Exercise",
		level:     "Level 7/8",
		score:     9.125,
	})
	want := "RoutineID_16_Alyssa_Rantino_Floor_Exercise_Level_7_8_Score_9.125_ttdceq.webm"
	if got != want {
		t.Fatalf("buildFilename() = %q, want %q", got, want)
	}
}

func TestBuildFilenameOmitsMissingMetadata(t *testing.T) {
	got := buildFilename(16, "ttdceq", ".webm", routine{apparatus: "Vault"})
	want := "RoutineID_16_Vault_ttdceq.webm"
	if got != want {
		t.Fatalf("buildFilename() = %q, want %q", got, want)
	}
}

func TestLegacyFilenamePattern(t *testing.T) {
	for _, name := range []string{
		"RoutineID_16_ttdceq",
		"RoutineID_792_6cgzl7",
		"RoutineID_21_Floor_llsdyr",
		"RoutineID_22_Vault_Jane_Doe_101_abc123",
	} {
		if !legacyFilename.MatchString(name) {
			t.Errorf("legacyFilename does not match %q", name)
		}
	}
	for _, name := range []string{"RoutineID_xx_ttdceq", "RoutineID_16_short", "RoutineID_16_Floor_short"} {
		if legacyFilename.MatchString(name) {
			t.Errorf("legacyFilename unexpectedly matches %q", name)
		}
	}
}
