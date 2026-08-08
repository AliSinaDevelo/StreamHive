package lifecycle

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthorityAllocatesAndResumesAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	ctx := context.Background()

	first, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{Observed: Version{Epoch: 9}})
	require.NoError(t, err)
	versionOne, err := first.Next(ctx, Version{})
	require.NoError(t, err)
	versionTwo, err := first.Next(ctx, Version{})
	require.NoError(t, err)
	assert.Equal(t, Version{Epoch: 9, Sequence: 1}, versionOne)
	assert.Equal(t, Version{Epoch: 9, Sequence: 2}, versionTwo)

	restarted, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{})
	require.NoError(t, err)
	versionThree, err := restarted.Next(ctx, Version{})
	require.NoError(t, err)
	assert.Equal(t, Version{Epoch: 9, Sequence: 3}, versionThree)
}

func TestAuthorityAdoptsNewerObservedJournalTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	ctx := context.Background()
	authority, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{Observed: Version{Epoch: 3, Sequence: 4}})
	require.NoError(t, err)

	version, err := authority.Next(ctx, Version{Epoch: 7, Sequence: 11})
	require.NoError(t, err)
	assert.Equal(t, Version{Epoch: 7, Sequence: 12}, version)

	restarted, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{})
	require.NoError(t, err)
	assert.Equal(t, version, restarted.Current())
}

func TestAuthorityRejectsIdentityMismatchAndCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	ctx := context.Background()
	_, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{Observed: Version{Epoch: 1}})
	require.NoError(t, err)

	_, err = OpenAuthority(ctx, path, "node-b", AuthorityOptions{})
	assert.ErrorIs(t, err, ErrAuthorityMismatch)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err = OpenAuthority(ctx, path, "node-a", AuthorityOptions{})
	assert.ErrorIs(t, err, ErrAuthorityChecksum)
}

func TestAuthorityCancellationDoesNotAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	ctx := context.Background()
	authority, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{Observed: Version{Epoch: 1}})
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = authority.Next(canceled, Version{})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Version{Epoch: 1}, authority.Current())
}

func TestAuthorityAdvancesEpochAfterSequenceExhaustion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority")
	ctx := context.Background()
	authority, err := OpenAuthority(ctx, path, "node-a", AuthorityOptions{Observed: Version{Epoch: 4, Sequence: math.MaxUint64}})
	require.NoError(t, err)

	version, err := authority.Next(ctx, Version{})
	require.NoError(t, err)
	assert.Equal(t, Version{Epoch: 5, Sequence: 1}, version)

	exhaustedPath := filepath.Join(t.TempDir(), "authority")
	exhausted, err := OpenAuthority(ctx, exhaustedPath, "node-a", AuthorityOptions{Observed: Version{Epoch: math.MaxUint64, Sequence: math.MaxUint64}})
	require.NoError(t, err)
	_, err = exhausted.Next(ctx, Version{})
	assert.ErrorIs(t, err, ErrAuthorityExhausted)
}
