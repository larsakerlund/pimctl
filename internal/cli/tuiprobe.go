//go:build tuiprobe

// This file exists only to drive the interactive picker from an expect(1) pty
// test. It is behind the `tuiprobe` build tag, so none of it is compiled into
// the shipped pimctl binary.
//
// The picker cannot be exercised by an ordinary Go test: huh needs a real
// terminal, and its filtering, toggling and prefill behaviour are exactly the
// parts that broke in practice.

package cli

import (
	"fmt"

	"github.com/larsakerlund/pimctl/internal/armclient"
)

// probeRows builds a picker list shaped like the real tenant: a role name
// repeated across scopes, and three management groups sharing one display name
// — the collision that made huh toggle several rows at once.
func probeRows() []row {
	mkRow := func(role, roleGUID, scope, scopeName string) row {
		e := armclient.Eligibility{}
		e.Properties.Scope = scope
		e.Properties.RoleDefinitionID = scope + "/providers/Microsoft.Authorization/roleDefinitions/" + roleGUID
		e.Properties.RoleEligibilityScheduleID = scope + "/providers/Microsoft.Authorization/roleEligibilitySchedules/" + roleGUID
		e.Properties.MemberType = "Group"
		e.Properties.ExpandedProperties.RoleDefinition = armclient.Named{DisplayName: role}
		e.Properties.ExpandedProperties.Scope = armclient.Named{
			DisplayName: scopeName,
			Type:        "managementgroup",
			ID:          scope,
		}
		return row{Context: "contoso", Elig: e}
	}
	mgs := []string{"contoso-prod", "contoso-qa", "contoso-test"}
	roles := []string{
		"Contributor", "Owner", "Storage Blob Data Owner", "Storage Blob Data Contributor",
		"Key Vault Administrator", "Role Based Access Control Administrator",
	}
	rows := make([]row, 0, len(mgs)*len(roles)+2)
	for _, role := range roles {
		for _, mg := range mgs {
			rows = append(
				rows,
				mkRow(
					role,
					"guid-"+role,
					"/providers/Microsoft.Management/managementGroups/"+mg,
					"Contoso landing zones",
				),
			)
		}
	}
	// The filter target: two rows only.
	rows = append(
		rows,
		mkRow(
			"Cost Management Contributor",
			"434105ed",
			"/providers/Microsoft.Management/managementGroups/contoso-prod",
			"Contoso landing zones",
		),
		mkRow(
			"Cost Management Contributor",
			"434105ed",
			"/providers/Microsoft.Management/managementGroups/contoso-test",
			"Contoso landing zones",
		),
	)
	sortRows(rows)
	return rows
}

// ProbeSelect runs the genuine multi-select and prints a machine-readable
// summary of what came back.
func ProbeSelect() error {
	rows := probeRows()
	fmt.Printf("PROBE_ROWS=%d\n", len(rows))
	picked, err := selectInteractive(rows, false, scopeLabelerForRows(rows))
	if err != nil {
		fmt.Printf("PROBE_ERROR=%v\n", err)
		return err
	}
	fmt.Printf("PROBE_SELECTED_COUNT=%d\n", len(picked))
	for _, r := range picked {
		fmt.Printf("PROBE_SELECTED=%s @ %s\n", r.Elig.RoleName(), scopeLeaf(r.Elig.Properties.Scope))
	}
	fmt.Println("PROBE_DONE")
	return nil
}

// ProbeJustification runs the genuine justification prompt with a prefill, the
// way `activate` does from the state file.
func ProbeJustification(prefill string) error {
	fmt.Printf("PROBE_PREFILL=%s\n", prefill)
	got, err := promptJustification(prefill, "required by 2 of 3 roles you selected")
	if err != nil {
		fmt.Printf("PROBE_ERROR=%v\n", err)
		return err
	}
	fmt.Printf("PROBE_JUSTIFICATION=%s\n", got)
	fmt.Println("PROBE_DONE")
	return nil
}
