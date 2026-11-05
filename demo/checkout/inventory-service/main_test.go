package main

import "testing"

func TestPortFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty uses default", value: "", want: "8082"},
		{name: "valid port", value: "9092", want: "9092"},
		{name: "plus sign is normalized", value: "+82", want: "82"},
		{name: "not a number", value: "abc", wantErr: true},
		{name: "zero", value: "0", wantErr: true},
		{name: "too large", value: "70000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := portFromEnv(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("portFromEnv(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("portFromEnv(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
