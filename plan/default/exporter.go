// Copyright 2022 Block, Inc.

package default_plan

import "github.com/cashapp/blip"

func Exporter() blip.Plan {
	return blip.Plan{
		Name:   blip.DEFAULT_EXPORTER_PLAN,
		Source: "blip",
		Levels: map[string]blip.Level{
			"prom": blip.Level{ // key name and
				Name: "prom", // level Name must be equal
				Freq: "0",    // none, pulled/scaped on demand
				Collect: map[string]blip.Domain{
					"status.global": {
						Name: "status.global",
						Options: map[string]string{
							"all": "yes",
						},
					},
					"var.global": {
						Name: "var.global",
						Options: map[string]string{
							"all": "yes",
						},
					},
					"innodb": {
						Name: "innodb",
						Options: map[string]string{
							"all": "enabled",
						},
					},
				},
			},
		},
	}
}
