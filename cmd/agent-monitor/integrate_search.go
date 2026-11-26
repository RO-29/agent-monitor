package main

// Feeds search-index data into ops.go (health, repeat-failure monitor, intel).
func init() {
	searchStatusFn = func() any {
		if searchIx == nil {
			return nil
		}
		return searchIx.status()
	}
	repeatErrorsFn = func(since int64) []struct {
		Sig, Sample, SessionID string
		Count                  int
	} {
		var out []struct {
			Sig, Sample, SessionID string
			Count                  int
		}
		if searchIx == nil {
			return out
		}
		rows, err := searchIx.r.Query(`SELECT errSig, MAX(errText), MAX(session), COUNT(*) AS n FROM spans
			WHERE err = 1 AND ts >= ? AND errSig != '' AND fam = 'bash'
			AND NOT (cmdKey GLOB 'rg*' OR cmdKey GLOB 'grep*' OR cmdKey GLOB 'ls*' OR cmdKey GLOB 'cat*' OR cmdKey GLOB 'find*' OR cmdKey GLOB 'sed*' OR cmdKey GLOB 'head*' OR cmdKey GLOB 'test*' OR cmdKey GLOB 'which*')
			AND errSig NOT LIKE '%exit code%' GROUP BY errSig ORDER BY n DESC LIMIT 10`, since)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var r struct {
				Sig, Sample, SessionID string
				Count                  int
			}
			if rows.Scan(&r.Sig, &r.Sample, &r.SessionID, &r.Count) == nil {
				out = append(out, r)
			}
		}
		return out
	}
}
