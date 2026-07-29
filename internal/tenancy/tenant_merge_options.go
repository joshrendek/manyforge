package tenancy

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TenantMergeDestination is one active business beneath a separately owned
// destination master. HierarchyPath is ordered master-to-node so similarly
// named businesses remain distinguishable in the dashboard.
type TenantMergeDestination struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	TenantRootID   uuid.UUID `json:"tenant_root_id"`
	TenantRootName string    `json:"tenant_root_name"`
	HierarchyPath  string    `json:"hierarchy_path"`
	IsTenantRoot   bool      `json:"is_tenant_root"`
}

// TenantMergeSourceOptions contains only source masters for which the current
// human is a direct built-in Owner and at least one authorized destination
// exists in another active root.
type TenantMergeSourceOptions struct {
	SourceRootID   uuid.UUID                `json:"source_root_id"`
	SourceRootName string                   `json:"source_root_name"`
	Destinations   []TenantMergeDestination `json:"destinations"`
}

// ListTenantMergeOptions returns the exact source/destination pairs that can
// begin preflight. The query runs with the caller's principal context and also
// requires direct built-in ownership at both roots plus hierarchy.manage at
// every destination. Hidden and inactive tenants never enter the result.
func (s *Service) ListTenantMergeOptions(
	ctx context.Context,
	actorID uuid.UUID,
) ([]TenantMergeSourceOptions, error) {
	options := []TenantMergeSourceOptions{}
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH direct_owner_roots AS (
			    SELECT root.id, root.name
			    FROM business root
			    JOIN membership membership
			      ON membership.business_id = root.id
			     AND membership.tenant_root_id = root.id
			     AND membership.principal_id = $1
			    JOIN role owner_role
			      ON owner_role.id = membership.role_id
			     AND owner_role.tenant_root_id IS NULL
			     AND owner_role.key = 'owner'
			     AND owner_role.is_locked
			    JOIN principal actor
			      ON actor.id = $1
			     AND actor.kind = 'human'
			    WHERE root.id = root.tenant_root_id
			      AND root.parent_id IS NULL
			      AND root.status = 'active'
			      AND root.deleted_at IS NULL
			),
			permitted_destinations AS (
			    SELECT destination.id,
			           destination.name,
			           destination.tenant_root_id,
			           destination.parent_id
			    FROM business destination
			    JOIN businesses_with_permission($1, 'hierarchy.manage') permitted
			      ON permitted.business_id = destination.id
			    JOIN direct_owner_roots destination_root
			      ON destination_root.id = destination.tenant_root_id
			    WHERE destination.status = 'active'
			      AND destination.deleted_at IS NULL
			),
			destination_paths AS (
			    SELECT destination.id,
			           string_agg(ancestor.name, ' / ' ORDER BY closure.depth DESC)
			             AS hierarchy_path
			    FROM permitted_destinations destination
			    JOIN business_closure closure
			      ON closure.descendant_id = destination.id
			     AND closure.tenant_root_id = destination.tenant_root_id
			    JOIN business ancestor ON ancestor.id = closure.ancestor_id
			    GROUP BY destination.id
			)
			SELECT source.id,
			       source.name,
			       destination.id,
			       destination.name,
			       destination.tenant_root_id,
			       destination_root.name,
			       path.hierarchy_path,
			       destination.parent_id IS NULL
			FROM direct_owner_roots source
			JOIN permitted_destinations destination
			  ON destination.tenant_root_id <> source.id
			JOIN direct_owner_roots destination_root
			  ON destination_root.id = destination.tenant_root_id
			JOIN destination_paths path ON path.id = destination.id
			ORDER BY lower(source.name), source.id,
			         lower(destination_root.name), lower(path.hierarchy_path),
			         destination.id`,
			actorID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		sourceIndexes := make(map[uuid.UUID]int)
		for rows.Next() {
			var sourceID uuid.UUID
			var sourceName string
			var destination TenantMergeDestination
			if err := rows.Scan(
				&sourceID,
				&sourceName,
				&destination.ID,
				&destination.Name,
				&destination.TenantRootID,
				&destination.TenantRootName,
				&destination.HierarchyPath,
				&destination.IsTenantRoot,
			); err != nil {
				return err
			}
			index, ok := sourceIndexes[sourceID]
			if !ok {
				index = len(options)
				sourceIndexes[sourceID] = index
				options = append(options, TenantMergeSourceOptions{
					SourceRootID:   sourceID,
					SourceRootName: sourceName,
					Destinations:   []TenantMergeDestination{},
				})
			}
			options[index].Destinations = append(
				options[index].Destinations,
				destination,
			)
		}
		return rows.Err()
	})
	return options, err
}
