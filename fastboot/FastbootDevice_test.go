package fastboot

import "testing"

func TestRecvFinalResponseCollectsInfo(t *testing.T) {
	responses := []struct {
		status FastbootResponseStatus
		data   []byte
	}{
		{status: Status.INFO, data: []byte("partition-size:boot:0x4000000")},
		{status: Status.INFO, data: []byte("partition-type:boot:raw")},
		{status: Status.OKAY, data: []byte{}},
	}
	index := 0

	status, data, infos, err := recvFinalResponse(func() (FastbootResponseStatus, []byte, error) {
		resp := responses[index]
		index++
		return resp.status, resp.data, nil
	})
	if err != nil {
		t.Fatalf("recvFinalResponse() error = %v", err)
	}
	if status != Status.OKAY {
		t.Fatalf("unexpected final status: got %q want %q", status, Status.OKAY)
	}
	if len(data) != 0 {
		t.Fatalf("unexpected final payload: %q", string(data))
	}
	if len(infos) != 2 {
		t.Fatalf("unexpected info count: got %d want 2", len(infos))
	}
	if string(infos[0]) != "partition-size:boot:0x4000000" {
		t.Fatalf("unexpected first info line: %q", string(infos[0]))
	}
	if string(infos[1]) != "partition-type:boot:raw" {
		t.Fatalf("unexpected second info line: %q", string(infos[1]))
	}
}

func TestRecvFinalResponseReturnsError(t *testing.T) {
	expected := Error.VarNotFound

	status, data, infos, err := recvFinalResponse(func() (FastbootResponseStatus, []byte, error) {
		return Status.FAIL, []byte("missing"), expected
	})
	if err == nil {
		t.Fatalf("recvFinalResponse() error = nil, want %v", expected)
	}
	if err != expected {
		t.Fatalf("recvFinalResponse() error = %v, want %v", err, expected)
	}
	if status != Status.FAIL {
		t.Fatalf("unexpected final status: got %q want %q", status, Status.FAIL)
	}
	if data != nil {
		t.Fatalf("unexpected final payload: %q", string(data))
	}
	if len(infos) != 0 {
		t.Fatalf("unexpected info count: got %d want 0", len(infos))
	}
}

func TestNormalizeSlotSuffix(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain slot", input: "a", want: "_a"},
		{name: "prefixed slot", input: "_b", want: "_b"},
		{name: "mixed case", input: " A ", want: "_a"},
		{name: "numeric slot", input: "0", want: "_0"},
		{name: "empty", input: "", wantErr: true},
		{name: "invalid chars", input: "a-b", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeSlotSuffix(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeSlotSuffix(%q) = %q, want error", tt.input, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("normalizeSlotSuffix(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeSlotSuffix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
