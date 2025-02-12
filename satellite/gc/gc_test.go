// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package gc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"storj.io/common/encryption"
	"storj.io/common/memory"
	"storj.io/common/paths"
	"storj.io/common/pb"
	"storj.io/common/storj"
	"storj.io/common/testcontext"
	"storj.io/common/testrand"
	"storj.io/storj/private/testplanet"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/gc"
	"storj.io/storj/satellite/metabase"
	"storj.io/storj/storage"
	"storj.io/storj/storagenode"
	"storj.io/uplink/private/testuplink"
)

// TestGarbageCollection does the following:
// * Set up a network with one storagenode
// * Upload two objects
// * Delete one object from the metainfo service on the satellite
// * Wait for bloom filter generation
// * Check that pieces of the deleted object are deleted on the storagenode
// * Check that pieces of the kept object are not deleted on the storagenode.
func TestGarbageCollection(t *testing.T) {
	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: 1, UplinkCount: 1,
		Reconfigure: testplanet.Reconfigure{
			Satellite: func(log *zap.Logger, index int, config *satellite.Config) {
				config.GarbageCollection.FalsePositiveRate = 0.000000001
				config.GarbageCollection.Interval = 500 * time.Millisecond
			},
			StorageNode: func(index int, config *storagenode.Config) {
				config.Retain.MaxTimeSkew = 0
			},
		},
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		satellite := planet.Satellites[0]
		upl := planet.Uplinks[0]
		targetNode := planet.StorageNodes[0]
		gcService := satellite.GarbageCollection.Service
		gcService.Loop.Pause()

		// Upload two objects
		testData1 := testrand.Bytes(8 * memory.KiB)
		testData2 := testrand.Bytes(8 * memory.KiB)

		err := upl.Upload(ctx, satellite, "testbucket", "test/path/1", testData1)
		require.NoError(t, err)

		objectLocationToDelete, segmentToDelete := getSegment(ctx, t, satellite, upl, "testbucket", "test/path/1")

		var deletedPieceID storj.PieceID
		for _, p := range segmentToDelete.Pieces {
			if p.StorageNode == targetNode.ID() {
				deletedPieceID = segmentToDelete.RootPieceID.Derive(p.StorageNode, int32(p.Number))
				break
			}
		}
		require.NotZero(t, deletedPieceID)

		err = upl.Upload(ctx, satellite, "testbucket", "test/path/2", testData2)
		require.NoError(t, err)
		_, segmentToKeep := getSegment(ctx, t, satellite, upl, "testbucket", "test/path/2")
		var keptPieceID storj.PieceID
		for _, p := range segmentToKeep.Pieces {
			if p.StorageNode == targetNode.ID() {
				keptPieceID = segmentToKeep.RootPieceID.Derive(p.StorageNode, int32(p.Number))
				break
			}
		}
		require.NotZero(t, keptPieceID)

		// Delete one object from metainfo service on satellite
		_, err = satellite.Metabase.DB.DeleteObjectsAllVersions(ctx, metabase.DeleteObjectsAllVersions{
			Locations: []metabase.ObjectLocation{objectLocationToDelete},
		})
		require.NoError(t, err)

		// Check that piece of the deleted object is on the storagenode
		pieceAccess, err := targetNode.DB.Pieces().Stat(ctx, storage.BlobRef{
			Namespace: satellite.ID().Bytes(),
			Key:       deletedPieceID.Bytes(),
		})
		require.NoError(t, err)
		require.NotNil(t, pieceAccess)

		// The pieceInfo.GetPieceIDs query converts piece creation and the filter creation timestamps
		// to datetime in sql. This chops off all precision beyond seconds.
		// In this test, the amount of time that elapses between piece uploads and the gc loop is
		// less than a second, meaning datetime(piece_creation) < datetime(filter_creation) is false unless we sleep
		// for a second.
		time.Sleep(1 * time.Second)

		// Wait for next iteration of garbage collection to finish
		gcService.Loop.Restart()
		gcService.Loop.TriggerWait()

		// Wait for the storagenode's RetainService queue to be empty
		targetNode.Storage2.RetainService.TestWaitUntilEmpty()

		// Check that piece of the deleted object is not on the storagenode
		pieceAccess, err = targetNode.DB.Pieces().Stat(ctx, storage.BlobRef{
			Namespace: satellite.ID().Bytes(),
			Key:       deletedPieceID.Bytes(),
		})
		require.Error(t, err)
		require.Nil(t, pieceAccess)

		// Check that piece of the kept object is on the storagenode
		pieceAccess, err = targetNode.DB.Pieces().Stat(ctx, storage.BlobRef{
			Namespace: satellite.ID().Bytes(),
			Key:       keptPieceID.Bytes(),
		})
		require.NoError(t, err)
		require.NotNil(t, pieceAccess)
	})
}

func getSegment(ctx *testcontext.Context, t *testing.T, satellite *testplanet.Satellite, upl *testplanet.Uplink, bucket, path string) (_ metabase.ObjectLocation, _ metabase.Segment) {
	access := upl.Access[satellite.ID()]

	serializedAccess, err := access.Serialize()
	require.NoError(t, err)

	store, err := encryptionAccess(serializedAccess)
	require.NoError(t, err)

	encryptedPath, err := encryption.EncryptPathWithStoreCipher(bucket, paths.NewUnencrypted(path), store)
	require.NoError(t, err)

	objectLocation :=
		metabase.ObjectLocation{
			ProjectID:  upl.Projects[0].ID,
			BucketName: "testbucket",
			ObjectKey:  metabase.ObjectKey(encryptedPath.Raw()),
		}

	lastSegment, err := satellite.Metabase.DB.GetLatestObjectLastSegment(ctx, metabase.GetLatestObjectLastSegment{
		ObjectLocation: objectLocation,
	})
	require.NoError(t, err)

	return objectLocation, lastSegment
}

func encryptionAccess(access string) (*encryption.Store, error) {
	data, version, err := base58.CheckDecode(access)
	if err != nil || version != 0 {
		return nil, errors.New("invalid access grant format")
	}

	p := new(pb.Scope)
	if err := pb.Unmarshal(data, p); err != nil {
		return nil, err
	}

	key, err := storj.NewKey(p.EncryptionAccess.DefaultKey)
	if err != nil {
		return nil, err
	}

	store := encryption.NewStore()
	store.SetDefaultKey(key)
	store.SetDefaultPathCipher(storj.EncAESGCM)

	return store, nil
}

func TestGarbageCollection_PendingObject(t *testing.T) {
	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: 1, UplinkCount: 1,
		Reconfigure: testplanet.Reconfigure{
			Satellite: testplanet.Combine(
				func(log *zap.Logger, index int, config *satellite.Config) {
					config.GarbageCollection.FalsePositiveRate = 0.000000001
					config.GarbageCollection.Interval = 500 * time.Millisecond
				},
				testplanet.MaxSegmentSize(20*memory.KiB),
			),
			StorageNode: func(index int, config *storagenode.Config) {
				config.Retain.MaxTimeSkew = 0
			},
		},
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		satellite := planet.Satellites[0]
		upl := planet.Uplinks[0]

		testData := testrand.Bytes(15 * memory.KiB)
		pendingStreamID := startMultipartUpload(ctx, t, upl, satellite, "testbucket", "multi", testData)

		segments, err := satellite.Metabase.DB.TestingAllSegments(ctx)
		require.NoError(t, err)
		require.Len(t, segments, 1)
		require.Len(t, segments[0].Pieces, 1)

		// The pieceInfo.GetPieceIDs query converts piece creation and the filter creation timestamps
		// to datetime in sql. This chops off all precision beyond seconds.
		// In this test, the amount of time that elapses between piece uploads and the gc loop is
		// less than a second, meaning datetime(piece_creation) < datetime(filter_creation) is false unless we sleep
		// for a second.

		lastPieceCounts := map[storj.NodeID]int{}
		pieceTracker := gc.NewPieceTracker(satellite.Log.Named("gc observer"), gc.Config{
			FalsePositiveRate: 0.000000001,
			InitialPieces:     10,
		}, lastPieceCounts)

		err = satellite.Metabase.SegmentLoop.Join(ctx, pieceTracker)
		require.NoError(t, err)

		require.NotEmpty(t, pieceTracker.RetainInfos)
		info := pieceTracker.RetainInfos[planet.StorageNodes[0].ID()]
		require.NotNil(t, info)
		require.Equal(t, 1, info.Count)

		completeMultipartUpload(ctx, t, upl, satellite, "testbucket", "multi", pendingStreamID)
		gotData, err := upl.Download(ctx, satellite, "testbucket", "multi")
		require.NoError(t, err)
		require.Equal(t, testData, gotData)
	})
}

func startMultipartUpload(ctx context.Context, t *testing.T, uplink *testplanet.Uplink, satellite *testplanet.Satellite, bucketName string, path storj.Path, data []byte) string {
	_, found := testuplink.GetMaxSegmentSize(ctx)
	if !found {
		ctx = testuplink.WithMaxSegmentSize(ctx, satellite.Config.Metainfo.MaxSegmentSize)
	}

	project, err := uplink.GetProject(ctx, satellite)
	require.NoError(t, err)
	defer func() { require.NoError(t, project.Close()) }()

	_, err = project.EnsureBucket(ctx, bucketName)
	require.NoError(t, err)

	info, err := project.BeginUpload(ctx, bucketName, path, nil)
	require.NoError(t, err)

	upload, err := project.UploadPart(ctx, bucketName, path, info.UploadID, 1)
	require.NoError(t, err)
	_, err = upload.Write(data)
	require.NoError(t, err)
	require.NoError(t, upload.Commit())

	return info.UploadID
}

func completeMultipartUpload(ctx context.Context, t *testing.T, uplink *testplanet.Uplink, satellite *testplanet.Satellite, bucketName string, path storj.Path, streamID string) {
	_, found := testuplink.GetMaxSegmentSize(ctx)
	if !found {
		ctx = testuplink.WithMaxSegmentSize(ctx, satellite.Config.Metainfo.MaxSegmentSize)
	}

	project, err := uplink.GetProject(ctx, satellite)
	require.NoError(t, err)
	defer func() { require.NoError(t, project.Close()) }()

	_, err = project.CommitUpload(ctx, bucketName, path, streamID, nil)
	require.NoError(t, err)
}
