// Package meta: cluster-level metadata
/*
 * Copyright (c) 2018-2023, NVIDIA CORPORATION. All rights reserved.
 */
package meta

type (
	// RMD aka "rebalance metadata" is used to distribute information
	// for the next rebalance.
	RMD struct {
		Ext       any      `json:"ext,omitempty"` // within meta-version extensions
		Resilver  string   `json:"resilver,omitempty"`
		TargetIDs []string `json:"target_ids,omitempty"`
		Version   int64    `json:"version"`
	}
)