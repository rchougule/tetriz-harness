// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

func strptr(s string) *string { return &s }

func TestAggregator_CountSecurityIncidents(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	t.Run("sums counts across all flag types", func(t *testing.T) {
		t.Parallel()
		agg, m := newAggregatorMocks(t, nil)
		m.security.EXPECT().
			CountByFlagType(gomock.Any(), aggTestOrgID, from, to).
			Return(map[string]int{"api-key-leak": 2, "pii-injection": 3}, nil)

		got, err := agg.CountSecurityIncidents(context.Background(), aggTestOrgID, from, to)
		require.NoError(t, err)
		assert.Equal(t, 5, got)
	})

	t.Run("no flags yields zero", func(t *testing.T) {
		t.Parallel()
		agg, m := newAggregatorMocks(t, nil)
		m.security.EXPECT().
			CountByFlagType(gomock.Any(), aggTestOrgID, from, to).
			Return(map[string]int{}, nil)

		got, err := agg.CountSecurityIncidents(context.Background(), aggTestOrgID, from, to)
		require.NoError(t, err)
		assert.Zero(t, got)
	})

	t.Run("propagates the DAO error", func(t *testing.T) {
		t.Parallel()
		agg, m := newAggregatorMocks(t, nil)
		m.security.EXPECT().
			CountByFlagType(gomock.Any(), aggTestOrgID, from, to).
			Return(nil, errors.New("db down"))

		_, err := agg.CountSecurityIncidents(context.Background(), aggTestOrgID, from, to)
		require.Error(t, err)
	})
}

func TestAggregator_ListInScopeProjects(t *testing.T) {
	t.Parallel()

	t.Run("returns only active repos that carry a fetched tree", func(t *testing.T) {
		t.Parallel()
		agg, m := newAggregatorMocks(t, nil)
		m.repo.EXPECT().
			ListAllActiveForOrg(gomock.Any(), aggTestOrgID).
			Return([]*model.Repository{
				{ID: "repo-with-tree", TreeKey: strptr("repo-trees/x.json")},
				{ID: "repo-no-tree", TreeKey: nil},
				{ID: "repo-empty-tree", TreeKey: strptr("")},
			}, nil)

		got, err := agg.ListInScopeProjects(context.Background(), aggTestOrgID)
		require.NoError(t, err)
		gotIDs := make([]string, 0, len(got))
		for _, repo := range got {
			gotIDs = append(gotIDs, repo.ID)
		}
		assert.Equal(t, []string{"repo-with-tree"}, gotIDs, "only the tree-carrying repo is in scope")
	})

	t.Run("propagates the DAO error", func(t *testing.T) {
		t.Parallel()
		agg, m := newAggregatorMocks(t, nil)
		m.repo.EXPECT().
			ListAllActiveForOrg(gomock.Any(), aggTestOrgID).
			Return(nil, errors.New("db down"))

		_, err := agg.ListInScopeProjects(context.Background(), aggTestOrgID)
		require.Error(t, err)
	})
}
