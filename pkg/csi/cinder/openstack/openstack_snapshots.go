/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package openstack snapshots provides an implementation of Cinder Snapshot features
// cinder functions using Gophercloud.
package openstack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/klog/v2"
)

const (
	snapshotReadyStatus = "available"
	snapReadyDuration   = 1 * time.Second
	snapReadyFactor     = 1.2
	snapReadySteps      = 10

	snapshotDescription      = "Created by OpenStack Cinder CSI driver"
	SnapshotForceCreate      = "force-create"
	SnapshotType             = "type"
	SnapshotAvailabilityZone = "availability"
)

// CreateSnapshot issues a request to take a Snapshot of the specified Volume with the corresponding ID and
// returns the resultant gophercloud Snapshot Item upon success
func (os *OpenStack) CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error) {

	force := false
	// if no flag given, then force will be false by default
	// if flag it given , check it
	if item, ok := (tags)[SnapshotForceCreate]; ok {
		var err error
		force, err = strconv.ParseBool(item)
		if err != nil {
			klog.V(5).Infof("Make force create flag to false due to: %v", err)
		}

		delete(tags, SnapshotForceCreate)
	}
	// Force the creation of snapshot even the Volume is in in-use state
	opts := &snapshots.CreateOpts{
		VolumeID:    volID,
		Name:        name,
		Description: snapshotDescription,
		Force:       force,
	}
	if tags != nil {
		opts.Metadata = tags
	}
	// TODO: Do some check before really call openstack API on the input
	mc := metrics.NewMetricContext("snapshot", "create")
	snap, err := snapshots.Create(ctx, os.blockstorage, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		return &snapshots.Snapshot{}, err
	}
	// There's little value in rewrapping these gophercloud types into yet another abstraction/type, instead just
	// return the gophercloud item
	return snap, nil
}

// ListSnapshots retrieves a list of active snapshots from Cinder for the corresponding Tenant.  We also
// provide the ability to provide limit and offset to enable the consumer to provide accurate pagination.
// In addition the filters argument provides a mechanism for passing in valid filter strings to the list
// operation.  Valid filter keys are:  Name, Status, VolumeID, Limit, Marker (TenantID has no effect)
func (os *OpenStack) ListSnapshots(ctx context.Context, filters map[string]string) ([]snapshots.Snapshot, string, error) {
	var nextPageToken string
	var snaps []snapshots.Snapshot

	// Build the Opts
	opts := snapshots.ListOpts{}
	for key, val := range filters {
		switch key {
		case "Status":
			opts.Status = val
		case "Name":
			opts.Name = val
		case "VolumeID":
			opts.VolumeID = val
		case "Marker":
			opts.Marker = val
		case "Limit":
			opts.Limit, _ = strconv.Atoi(val)
		default:
			klog.V(3).Infof("Not a valid filter key %s", key)
		}
	}
	mc := metrics.NewMetricContext("snapshot", "list")
	err := snapshots.List(os.blockstorage, opts).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		var err error

		snaps, err = snapshots.ExtractSnapshots(page)
		if err != nil {
			return false, err
		}

		nextPageURL, err := page.NextPageURL()
		if err != nil {
			return false, err
		}

		if nextPageURL != "" {
			pageURL, err := url.Parse(nextPageURL)
			if err != nil {
				return false, err
			}
			nextPageToken = pageURL.Query().Get("marker")
		}

		return false, nil
	})
	if mc.ObserveRequest(err) != nil {
		return nil, nextPageToken, err
	}

	return snaps, nextPageToken, nil
}

// DeleteSnapshot issues a request to delete the Snapshot with the specified ID from the Cinder backend
func (os *OpenStack) DeleteSnapshot(ctx context.Context, snapID string) error {
	mc := metrics.NewMetricContext("snapshot", "delete")
	err := snapshots.Delete(ctx, os.blockstorage, snapID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		klog.Errorf("Failed to delete snapshot: %v", err)
	}
	return err
}

// GetSnapshotByID returns snapshot details by id
func (os *OpenStack) GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error) {
	mc := metrics.NewMetricContext("snapshot", "get")
	s, err := snapshots.Get(ctx, os.blockstorage, snapshotID).Extract()
	if mc.ObserveRequest(err) != nil {
		klog.Errorf("Failed to get snapshot: %v", err)
		return nil, err
	}
	return s, nil
}

// WaitSnapshotReady waits till snapshot is ready
func (os *OpenStack) WaitSnapshotReady(ctx context.Context, snapshotID string) (string, error) {
	backoff := wait.Backoff{
		Duration: snapReadyDuration,
		Factor:   snapReadyFactor,
		Steps:    snapReadySteps,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		ready, err := os.snapshotIsReady(ctx, snapshotID)
		if err != nil {
			return false, err
		}
		return ready, nil
	})

	if wait.Interrupted(err) {
		err = fmt.Errorf("timeout, Snapshot %s is still not Ready %v", snapshotID, err)
	}

	snap, _ := os.GetSnapshotByID(ctx, snapshotID)

	if snap != nil {
		return snap.Status, err
	} else {
		return "Failed to get snapshot status", err
	}
}

func (os *OpenStack) snapshotIsReady(ctx context.Context, snapshotID string) (bool, error) {
	snap, err := os.GetSnapshotByID(ctx, snapshotID)
	if err != nil {
		return false, err
	}

	return snap.Status == snapshotReadyStatus, nil
}
