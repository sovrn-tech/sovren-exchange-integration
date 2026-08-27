package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractGoVersion(t *testing.T) {
	cases := map[string]string{
		"go version go1.25.12 linux/amd64": "1.25.12",
		"go1.25.12":                        "1.25.12",
		"1.25.12":                          "1.25.12",
		"go version go1.24 darwin/arm64":   "1.24",
		"":                                 "",
	}
	for in, want := range cases {
		require.Equal(t, want, extractGoVersion(in), "extractGoVersion(%q)", in)
	}
}

func TestBuildDepPrefixMatch(t *testing.T) {
	ni := &restNodeInfo{BuildDeps: map[string]string{
		"github.com/cosmos/ibc-go/v10": "v10.5.0",
		"github.com/CosmWasm/wasmd":    "v0.60.7",
		"github.com/cosmos/cosmos-sdk": "v0.53.8",
	}}
	v, ok := ni.buildDep("github.com/cosmos/ibc-go/") // prefix survives a v10->v11 bump
	require.True(t, ok)
	require.Equal(t, "v10.5.0", v)
	v, ok = ni.buildDep("github.com/CosmWasm/wasmd")
	require.True(t, ok)
	require.Equal(t, "v0.60.7", v)
	_, ok = ni.buildDep("github.com/nope/absent")
	require.False(t, ok)
}

func TestCheckLiveBuildDeps(t *testing.T) {
	base := &restNodeInfo{
		GoVersion: "go version go1.25.12 linux/amd64",
		BuildDeps: map[string]string{
			"github.com/cosmos/ibc-go/v10": "v10.5.0",
			"github.com/CosmWasm/wasmd":    "v0.60.7",
		},
	}

	t.Run("exact deps + newer go patch: no problems, one go skew note", func(t *testing.T) {
		p, n := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.25.7", base)
		require.Empty(t, p)
		require.Len(t, n, 1)
		require.Contains(t, n[0], "newer toolchain patch")
	})

	t.Run("ibc_go mismatch is a hard problem", func(t *testing.T) {
		p, _ := checkLiveBuildDeps("v10.4.0", "v0.60.7", "1.25.7", base)
		require.Len(t, p, 1)
		require.Contains(t, p[0], "ibc_go")
		require.Contains(t, p[0], "build_deps")
	})

	t.Run("cosmwasm mismatch is a hard problem", func(t *testing.T) {
		p, _ := checkLiveBuildDeps("v10.5.0", "v0.60.1", "1.25.7", base)
		require.Len(t, p, 1)
		require.Contains(t, p[0], "cosmwasm_wasmd")
	})

	t.Run("go on a different minor line is a hard problem", func(t *testing.T) {
		p, _ := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.24.0", base)
		require.Len(t, p, 1)
		require.Contains(t, p[0], "minor line")
	})

	t.Run("go exact match: no note", func(t *testing.T) {
		ni := &restNodeInfo{GoVersion: "go1.25.7", BuildDeps: base.BuildDeps}
		p, n := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.25.7", ni)
		require.Empty(t, p)
		require.Empty(t, n)
	})

	t.Run("absent build_deps degrades to notes, never a fail", func(t *testing.T) {
		ni := &restNodeInfo{GoVersion: "go1.25.7"} // stripped binary
		p, n := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.25.7", ni)
		require.Empty(t, p)
		require.Len(t, n, 2)
		require.Contains(t, n[0], "build_deps unavailable")
	})

	t.Run("dep absent from a populated build_deps is a note", func(t *testing.T) {
		ni := &restNodeInfo{GoVersion: "go1.25.7", BuildDeps: map[string]string{
			"github.com/cosmos/ibc-go/v10": "v10.5.0",
		}}
		p, n := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.25.7", ni)
		require.Empty(t, p)
		require.Len(t, n, 1)
		require.Contains(t, n[0], "cosmwasm_wasmd")
		require.Contains(t, n[0], "absent from live build_deps")
	})

	t.Run("empty go_version records a note, not a silent skip", func(t *testing.T) {
		ni := &restNodeInfo{GoVersion: "", BuildDeps: base.BuildDeps}
		p, n := checkLiveBuildDeps("v10.5.0", "v0.60.7", "1.25.7", ni)
		require.Empty(t, p)
		require.Len(t, n, 1)
		require.Contains(t, n[0], "go_version unavailable")
	})
}
