package main

import "testing"

func TestPortFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty uses default", value: "", want: "8081"},
		{name: "valid port", value: "9091", want: "9091"},
		{name: "plus sign is normalized", value: "+81", want: "81"},
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

func TestPaymentClientFromEnv(t *testing.T) {
	c, err := paymentClientFromEnv("")
	if err != nil || c.BaseURL() != "http://localhost:8083" {
		t.Errorf("default: %v, %v", c, err)
	}
	c, err = paymentClientFromEnv("http://payments:9000/")
	if err != nil || c.BaseURL() != "http://payments:9000" {
		t.Errorf("custom: %v, %v", c, err)
	}
	for _, bad := range []string{"payments:9000", "ftp://payments", "http://payments/api"} {
		if _, err := paymentClientFromEnv(bad); err == nil {
			t.Errorf("paymentClientFromEnv(%q): want error", bad)
		}
	}
}

func TestInventoryClientFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty uses default", value: "", want: "http://localhost:8082"},
		{name: "custom URL", value: "http://127.0.0.1:9092/", want: "http://127.0.0.1:9092"},
		{name: "missing scheme", value: "localhost:8082", wantErr: true},
		{name: "path not allowed", value: "http://localhost:8082/api", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := inventoryClientFromEnv(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("inventoryClientFromEnv(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && c.BaseURL() != tt.want {
				t.Errorf("BaseURL() = %q, want %q", c.BaseURL(), tt.want)
			}
		})
	}
}
