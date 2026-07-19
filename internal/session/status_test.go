package session

import "testing"

func TestStatusValidation(t *testing.T) {
	if !StatusActive.Valid() {
		t.Errorf("expected StatusActive to be valid")
	}
	if Status("invalid").Valid() {
		t.Errorf("expected invalid status to be invalid")
	}
}

func TestValidateTransition(t *testing.T) {
	tests := []struct {
		from    Status
		to      Status
		wantErr bool
	}{
		{StatusActive, StatusPaused, false},
		{StatusActive, StatusCompleted, false},
		{StatusActive, StatusArchived, false},
		{StatusPaused, StatusActive, false},
		{StatusPaused, StatusCompleted, false},
		{StatusPaused, StatusArchived, false},
		{StatusCompleted, StatusActive, true},
		{StatusCompleted, StatusPaused, true},
		{StatusCompleted, StatusArchived, false},
		{StatusArchived, StatusActive, true},
		{StatusArchived, StatusPaused, true},
		{StatusArchived, StatusCompleted, true},
	}

	for _, tt := range tests {
		err := ValidateTransition(tt.from, tt.to)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateTransition(%s -> %s) error = %v, wantErr %v", tt.from, tt.to, err, tt.wantErr)
		}
	}
}
