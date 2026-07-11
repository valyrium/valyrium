package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// TestCLISubcommandDispatch pins how argv maps onto subcommands now that the
// binary has three of them (docs/adr/0002-tunnel-relay.md). The handlers are
// injected, so this exercises the dispatch table without starting a gateway,
// binding a port, or reaching for Let's Encrypt.
func TestCLISubcommandDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCmd  string
		wantArgs []string
		wantOut  string
		wantErr  string
	}{
		{
			// The gateway was what `valyrium` did before subcommands existed,
			// and it stays what `valyrium` does.
			name:     "no arguments runs the gateway",
			args:     nil,
			wantCmd:  "serve",
			wantArgs: []string{},
		},
		{
			name:     "a leading flag still runs the gateway",
			args:     []string{"--foo", "bar"},
			wantCmd:  "serve",
			wantArgs: []string{"--foo", "bar"},
		},
		{
			name:     "serve can be named explicitly",
			args:     []string{"serve"},
			wantCmd:  "serve",
			wantArgs: []string{},
		},
		{
			name:     "relay dispatches to the relay",
			args:     []string{"relay"},
			wantCmd:  "relay",
			wantArgs: []string{},
		},
		{
			name:     "tunnel dispatches to the tunnel",
			args:     []string{"tunnel"},
			wantCmd:  "tunnel",
			wantArgs: []string{},
		},
		{
			name:     "a subcommand is handed the arguments after its name",
			args:     []string{"tunnel", "--local", "127.0.0.1:9999"},
			wantCmd:  "tunnel",
			wantArgs: []string{"--local", "127.0.0.1:9999"},
		},
		{
			name:    "--version prints the version and runs nothing",
			args:    []string{"--version"},
			wantOut: "valyrium " + version,
		},
		{
			name:    "-v prints the version and runs nothing",
			args:    []string{"-v"},
			wantOut: "valyrium " + version,
		},
		{
			name:    "--help lists the subcommands",
			args:    []string{"--help"},
			wantOut: "valyrium relay",
		},
		{
			name:    "an unknown subcommand is an error, not a gateway",
			args:    []string{"bogus"},
			wantErr: `unknown subcommand "bogus"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				called   string
				gotArgs  []string
				commands = map[string]command{}
			)
			for _, name := range []string{"serve", "relay", "tunnel"} {
				commands[name] = func(args []string) error {
					called, gotArgs = name, args
					return nil
				}
			}

			var out bytes.Buffer
			err := run(tc.args, commands, &out)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error mentioning %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				if called != "" {
					t.Errorf("a failed dispatch still ran %q", called)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			if called != tc.wantCmd {
				t.Errorf("ran %q, want %q", called, tc.wantCmd)
			}
			if tc.wantCmd != "" && !slices.Equal(gotArgs, tc.wantArgs) {
				t.Errorf("arguments: got %q, want %q", gotArgs, tc.wantArgs)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q does not contain %q", out.String(), tc.wantOut)
			}
		})
	}

	// The table above proves the dispatcher routes; this proves the real
	// binary registers something to route to.
	t.Run("every documented subcommand is registered", func(t *testing.T) {
		registered := commands()
		for _, name := range []string{"serve", "relay", "tunnel"} {
			if registered[name] == nil {
				t.Errorf("subcommand %q is documented but not registered", name)
			}
			if !strings.Contains(usage, name) {
				t.Errorf("subcommand %q is registered but not in the usage text", name)
			}
		}
	})
}
