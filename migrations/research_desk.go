package migrations

import _ "embed"

// ResearchDeskEvents is installed by the running desk independently of the
// sequential bootstrap migrations until Wave10 numbering is reconciled.
//
//go:embed 0125_research_desk_events.sql
var ResearchDeskEvents string
