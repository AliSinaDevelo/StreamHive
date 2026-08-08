package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMembershipBookReplaceSortsAndRestores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "membership")
	book, err := OpenMembershipBook(path, MembershipOptions{})
	require.NoError(t, err)
	assert.False(t, book.Configured())

	require.NoError(t, book.Replace(ctx, []string{"node-b", "node-a"}))
	assert.True(t, book.Configured())
	assert.Equal(t, []string{"node-a", "node-b"}, book.Snapshot())

	restarted, err := OpenMembershipBook(path, MembershipOptions{})
	require.NoError(t, err)
	assert.True(t, restarted.Configured())
	assert.Equal(t, []string{"node-a", "node-b"}, restarted.Snapshot())

	require.NoError(t, restarted.Replace(ctx, []string{}))
	assert.True(t, restarted.Configured())
	assert.Empty(t, restarted.Snapshot())
}

func TestMembershipBookValidatesBoundariesAndChecksum(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "membership")
	book, err := OpenMembershipBook(path, MembershipOptions{MaxPeers: 1, MaxPeerIDBytes: 4})
	require.NoError(t, err)

	assert.ErrorIs(t, book.Replace(ctx, []string{"node-a", "node-b"}), ErrMembershipLimit)
	duplicateBook, err := OpenMembershipBook(filepath.Join(t.TempDir(), "membership"), MembershipOptions{})
	require.NoError(t, err)
	assert.ErrorIs(t, duplicateBook.Replace(ctx, []string{"node-a", "node-a"}), ErrMembershipDuplicate)
	assert.ErrorIs(t, book.Replace(ctx, []string{"node\n"}), ErrMembershipPeerInvalid)
	assert.ErrorIs(t, book.Replace(ctx, []string{"longer"}), ErrMembershipPeerInvalid)

	require.NoError(t, book.Replace(ctx, []string{"node"}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err = OpenMembershipBook(path, MembershipOptions{})
	assert.ErrorIs(t, err, ErrMembershipChecksum)

	corruptPath := filepath.Join(t.TempDir(), "membership")
	require.NoError(t, os.WriteFile(corruptPath, []byte("not a membership"), 0o600))
	_, err = OpenMembershipBook(corruptPath, MembershipOptions{})
	assert.ErrorIs(t, err, ErrMembershipCorrupt)
}

func TestMembershipBookRequiresDurableAcknowledgementForEveryMember(t *testing.T) {
	ctx := context.Background()
	membershipPath := filepath.Join(t.TempDir(), "membership")
	watermarkPath := filepath.Join(t.TempDir(), "watermarks")
	membership, err := OpenMembershipBook(membershipPath, MembershipOptions{})
	require.NoError(t, err)
	watermarks, err := OpenWatermarkBook(watermarkPath, WatermarkOptions{})
	require.NoError(t, err)
	target := Version{Epoch: 3, Sequence: 2}

	_, err = membership.WatermarksAt(ctx, watermarks, target)
	assert.ErrorIs(t, err, ErrMembershipNotConfigured)

	require.NoError(t, membership.Replace(ctx, []string{"node-a", "node-b"}))
	_, err = membership.WatermarksAt(ctx, watermarks, target)
	assert.ErrorIs(t, err, ErrMembershipBehind)

	require.NoError(t, watermarks.Acknowledge(ctx, "node-a", target))
	_, err = membership.WatermarksAt(ctx, watermarks, target)
	assert.ErrorIs(t, err, ErrMembershipBehind)

	require.NoError(t, watermarks.Acknowledge(ctx, "node-b", target))
	got, err := membership.WatermarksAt(ctx, watermarks, target)
	require.NoError(t, err)
	assert.Equal(t, []Version{target, target}, got)

	require.NoError(t, membership.Replace(ctx, nil))
	got, err = membership.WatermarksAt(ctx, watermarks, target)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMembershipBookWatermarksAtHonorsCancellationAndNilInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "membership")
	book, err := OpenMembershipBook(path, MembershipOptions{})
	require.NoError(t, err)
	assert.ErrorIs(t, book.Replace(ctx, []string{"node-a"}), context.Canceled)

	var configured *MembershipBook
	_, err = configured.WatermarksAt(context.Background(), nil, Version{Epoch: 1, Sequence: 1})
	assert.ErrorIs(t, err, ErrNilMembershipBook)

	membership, err := OpenMembershipBook(filepath.Join(t.TempDir(), "membership"), MembershipOptions{})
	require.NoError(t, err)
	require.NoError(t, membership.Replace(context.Background(), []string{"node-a"}))
	_, err = membership.WatermarksAt(context.Background(), nil, Version{Epoch: 1, Sequence: 1})
	assert.ErrorIs(t, err, ErrNilWatermarkBook)
	_, err = membership.WatermarksAt(context.Background(), &WatermarkBook{}, Version{})
	assert.ErrorIs(t, err, ErrMembershipWatermarkInvalid)
}
