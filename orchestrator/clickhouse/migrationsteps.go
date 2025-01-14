// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"akvorado/common/reporter"
	"akvorado/inlet/flow"
)

const (
	// flowsSchema is the canonical schema for flows table
	flowsSchema = `
 TimeReceived DateTime CODEC(DoubleDelta, LZ4),
 SamplingRate UInt64,
 ExporterAddress LowCardinality(IPv6),
 ExporterName LowCardinality(String),
 ExporterGroup LowCardinality(String),
 ExporterRole LowCardinality(String),
 ExporterSite LowCardinality(String),
 ExporterRegion LowCardinality(String),
 ExporterTenant LowCardinality(String),
 SrcAddr IPv6,
 DstAddr IPv6,
 SrcNetMask UInt8,
 DstNetMask UInt8,
 SrcAS UInt32,
 DstAS UInt32,
 SrcNetName LowCardinality(String),
 DstNetName LowCardinality(String),
 SrcNetRole LowCardinality(String),
 DstNetRole LowCardinality(String),
 SrcNetSite LowCardinality(String),
 DstNetSite LowCardinality(String),
 SrcNetRegion LowCardinality(String),
 DstNetRegion LowCardinality(String),
 SrcNetTenant LowCardinality(String),
 DstNetTenant LowCardinality(String),
 SrcCountry FixedString(2),
 DstCountry FixedString(2),
 DstASPath Array(UInt32),
 Dst1stAS UInt32,
 Dst2ndAS UInt32,
 Dst3rdAS UInt32,
 DstCommunities Array(UInt32),
 DstLargeCommunities Array(UInt128),
 InIfName LowCardinality(String),
 OutIfName LowCardinality(String),
 InIfDescription String,
 OutIfDescription String,
 InIfSpeed UInt32,
 OutIfSpeed UInt32,
 InIfConnectivity LowCardinality(String),
 OutIfConnectivity LowCardinality(String),
 InIfProvider LowCardinality(String),
 OutIfProvider LowCardinality(String),
 InIfBoundary Enum8('undefined' = 0, 'external' = 1, 'internal' = 2),
 OutIfBoundary Enum8('undefined' = 0, 'external' = 1, 'internal' = 2),
 EType UInt32,
 Proto UInt32,
 SrcPort UInt32,
 DstPort UInt32,
 Bytes UInt64,
 Packets UInt64,
 ForwardingStatus UInt32
`
)

// queryTableHash can be used to check if a table exists with the
// specified schema. This is not foolproof as it needs help if
// settings or populate query is changed.
func queryTableHash(hash uint64, more string) string {
	return fmt.Sprintf(`
SELECT bitAnd(v1, v2) FROM (
 SELECT 1 AS v1
 FROM system.tables
 WHERE name = $1 AND database = currentDatabase() %s
) t1, (
 SELECT groupBitXor(cityHash64(name,type,position)) == %d AS v2
 FROM system.columns
 WHERE table = $1 AND database = currentDatabase()
) t2`, more, hash)
}

// partialSchema returns the above schema minus some columns
func partialSchema(remove ...string) string {
	schema := []string{}
outer:
	for _, l := range strings.Split(flowsSchema, "\n") {
		for _, p := range remove {
			if strings.HasPrefix(strings.TrimSpace(l), fmt.Sprintf("%s ", p)) {
				continue outer
			}
		}
		schema = append(schema, l)
	}
	return strings.Join(schema, "\n")
}

// columnSpecToName extracts column name from its creation spec
func columnSpecToName(spec string) string {
	spec = strings.TrimPrefix(spec, "IF NOT EXISTS ")
	return strings.Split(spec, " ")[0]
}

// addColumnsAfter build a string to add columns after another column
func addColumnsAfter(after string, columns ...string) string {
	modifications := []string{}
	last := after
	for _, column := range columns {
		modifications = append(modifications, fmt.Sprintf("ADD COLUMN %s AFTER %s", column, last))
		last = columnSpecToName(column)
	}
	return strings.Join(modifications, ", ")
}

// appendToSortingKey returns a sorting key using the original one and
// adding the specified columns.
func appendToSortingKey(ctx context.Context, conn clickhouse.Conn, table string, columns ...string) (string, error) {
	row := conn.QueryRow(
		ctx,
		`SELECT sorting_key FROM system.tables WHERE name = $1 AND database = currentDatabase()`,
		table)
	if err := row.Err(); err != nil {
		return "", fmt.Errorf("cannot query sorting key for %q: %w", table, err)
	}
	var sortingKey string
	if err := row.Scan(&sortingKey); err != nil {
		return "", fmt.Errorf("unable to parse sorting key: %w", err)
	}
	return fmt.Sprintf("%s, %s", sortingKey, strings.Join(columns, ", ")), nil
}

// addColumnsAndUpdateSortingKey combines addColumnsAfter and appendToSortingKey
func addColumnsAndUpdateSortingKey(ctx context.Context, conn clickhouse.Conn, table string, after string, columns ...string) (string, error) {
	modifications := []string{addColumnsAfter(after, columns...)}
	columnNames := []string{}
	for _, column := range columns {
		columnNames = append(columnNames, columnSpecToName(column))
	}
	if table != "flows" {
		sortingKey, err := appendToSortingKey(ctx, conn, table, columnNames...)
		if err != nil {
			return "", err
		}
		modifications = append(modifications, fmt.Sprintf("MODIFY ORDER BY (%s)", sortingKey))
	}
	return strings.Join(modifications, ", "), nil
}

var nullMigrationStep = migrationStep{
	CheckQuery: `SELECT 1`,
	Args:       []interface{}{},
	Do:         func() error { return nil },
}

func (c *Component) migrationsStepCreateFlowsTable(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		if resolution.Interval == 0 {
			// Unconsolidated flows table
			partitionInterval := uint64((resolution.TTL / time.Duration(c.config.MaxPartitions)).Seconds())
			return migrationStep{
				CheckQuery: `SELECT 1 FROM system.tables WHERE name = $1 AND database = currentDatabase()`,
				Args:       []interface{}{"flows"},
				Do: func() error {
					return conn.Exec(ctx, fmt.Sprintf(`
CREATE TABLE flows (
%s
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDDhhmmss(toStartOfInterval(TimeReceived, INTERVAL %d second))
ORDER BY (TimeReceived, ExporterAddress, InIfName, OutIfName)`, flowsSchema, partitionInterval))
				},
			}
		}
		// Consolidated table. The ORDER BY clause excludes
		// field that are usually deduced from included
		// fields, assuming they won't change for the interval
		// of time considered. It excludes Bytes and Packets
		// that are summed. The order is the one we are most
		// likely to use when filtering. SrcAddr and DstAddr
		// are removed.
		tableName := fmt.Sprintf("flows_%s", resolution.Interval)
		viewName := fmt.Sprintf("%s_consumer", tableName)
		return migrationStep{
			CheckQuery: `SELECT 1 FROM system.tables WHERE name = $1 AND database = currentDatabase()`,
			Args:       []interface{}{tableName},
			Do: func() error {
				l.Debug().Msgf("drop flows consumer table for interval %s", resolution.Interval)
				err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s SYNC`, viewName))
				if err != nil {
					return fmt.Errorf("cannot drop flows consumer table for interval %s: %w",
						resolution.Interval, err)
				}

				partitionInterval := uint64((resolution.TTL / time.Duration(c.config.MaxPartitions)).Seconds())
				// Primary key does not cover all the sorting key as we cannot modify it
				// and it would impact performance negatively to have a too long
				// primary key. We do not use ExporterName, ExporterGroup, ... in
				// ORDER BY because we assume that for a value of ExporterAddress, they
				// are constant. The same applies for InIfDescription, InIfProvider (for
				// a value of ExporterAddress and InIfName, they are constant).
				// That's not the case for SrcNetName and others (they depend on the
				// SrcAddr which we don't have anymore).
				return conn.Exec(ctx, fmt.Sprintf(`
CREATE TABLE %s (
%s
)
ENGINE = SummingMergeTree((Bytes, Packets))
PARTITION BY toYYYYMMDDhhmmss(toStartOfInterval(TimeReceived, INTERVAL %d second))
PRIMARY KEY (TimeReceived,
          ExporterAddress,
          EType, Proto,
          InIfName, SrcAS, ForwardingStatus,
          OutIfName, DstAS,
          SamplingRate)
ORDER BY (TimeReceived,
          ExporterAddress,
          EType, Proto,
          InIfName, SrcAS, ForwardingStatus,
          OutIfName, DstAS,
          SamplingRate,
          SrcNetName, DstNetName,
          SrcNetRole, DstNetRole,
          SrcNetSite, DstNetSite,
          SrcNetRegion, DstNetRegion,
          SrcNetTenant, DstNetTenant,
          SrcCountry, DstCountry,
          Dst1stAS, Dst2ndAS, Dst3rdAS)`,
					tableName,
					partialSchema(
						"SrcAddr", "DstAddr",
						"SrcNetMask", "DstNetMask",
						"SrcPort", "DstPort",
						"DstASPath", "DstCommunities", "DstLargeCommunities"),
					partitionInterval))
			},
		}
	}
}

func (c *Component) migrationStepAddPacketSizeBucketColumn(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			tableName = "flows"
		} else {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
			Args: []interface{}{tableName, "PacketSizeBucket"},
			Do: func() error {
				boundaries := []int{64, 128, 256, 512, 768, 1024, 1280, 1501,
					2048, 3072, 4096, 8192, 10240, 16384, 32768, 65536}
				conditions := []string{}
				last := 0
				for _, boundary := range boundaries {
					conditions = append(conditions, fmt.Sprintf("PacketSize < %d, '%d-%d'",
						boundary, last, boundary-1))
					last = boundary
				}
				conditions = append(conditions, fmt.Sprintf("'%d-Inf'", last))
				return conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s %s",
					tableName, addColumnsAfter("Packets",
						"PacketSize UInt64 ALIAS intDiv(Bytes, Packets)",
						fmt.Sprintf("PacketSizeBucket LowCardinality(String) ALIAS multiIf(%s)",
							strings.Join(conditions, ", ")))))
			},
		}
	}
}

func (c *Component) migrationStepAddSrcNetNameDstNetNameColumns(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			tableName = "flows"
		} else {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
			Args: []interface{}{tableName, "DstNetName"},
			Do: func() error {
				modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, tableName,
					"DstAS",
					`SrcNetName LowCardinality(String)`,
					`DstNetName LowCardinality(String)`,
				)
				if err != nil {
					return err
				}
				return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s %s`,
					tableName, modifications))
			},
		}
	}
}

func (c *Component) migrationStepAddSrcNetNameDstNetOthersColumns(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			tableName = "flows"
		} else {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
			Args: []interface{}{tableName, "DstNetRole"},
			Do: func() error {
				modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, tableName,
					"DstNetName",
					`SrcNetRole LowCardinality(String)`,
					`DstNetRole LowCardinality(String)`,
					`SrcNetSite LowCardinality(String)`,
					`DstNetSite LowCardinality(String)`,
					`SrcNetRegion LowCardinality(String)`,
					`DstNetRegion LowCardinality(String)`,
					`SrcNetTenant LowCardinality(String)`,
					`DstNetTenant LowCardinality(String)`,
				)
				if err != nil {
					return err
				}
				return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s %s`,
					tableName, modifications))
			},
		}
	}
}

func (c *Component) migrationStepAddExporterColumns(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			tableName = "flows"
		} else {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
			Args: []interface{}{tableName, "ExporterTenant"},
			Do: func() error {
				return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s %s`,
					tableName, addColumnsAfter("ExporterGroup",
						`ExporterRole LowCardinality(String)`,
						`ExporterSite LowCardinality(String)`,
						`ExporterRegion LowCardinality(String)`,
						`ExporterTenant LowCardinality(String)`,
					)))
			},
		}
	}
}

func (c *Component) migrationStepFixOrderByCountry(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			return nullMigrationStep
		}
		tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.tables
WHERE name = $1 AND database = currentDatabase()
AND has(splitByRegexp(',\\s*', sorting_key), $2)`,
			Args: []interface{}{tableName, "SrcCountry"},
			Do: func() error {
				// Drop the columns
				l.Debug().Msg("drop SrcCountry/DstCountry columns")
				err := conn.Exec(ctx,
					fmt.Sprintf(`ALTER TABLE %s DROP COLUMN SrcCountry, DROP COLUMN DstCountry`,
						tableName))
				if err != nil {
					return fmt.Errorf("cannot drop SrcCountry/DstCountry columns: %w", err)
				}
				// Add them back
				l.Debug().Msg("add back SrcCountry/DstCountry columns")
				modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, tableName,
					"DstNetTenant",
					`SrcCountry FixedString(2)`,
					`DstCountry FixedString(2)`,
				)
				if err != nil {
					return err
				}
				return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s %s`,
					tableName, modifications))
			},
		}
	}
}

func (c *Component) migrationStepAddDstASPathColumns(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		var tableName string
		if resolution.Interval == 0 {
			tableName = "flows"
		} else {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
			Args: []interface{}{tableName, "Dst1stAS"},
			Do: func() error {
				var modifications string
				var err error
				if tableName == "flows" {
					// The flows table will get DstASPath, 1st, 2nd, 3rd ASN.
					modifications, err = addColumnsAndUpdateSortingKey(ctx, conn, tableName,
						"DstCountry",
						`DstASPath Array(UInt32)`,
						`Dst1stAS UInt32`,
						`Dst2ndAS UInt32`,
						`Dst3rdAS UInt32`,
					)
				} else {
					// The consolidated table will only get the three first ASNs.
					modifications, err = addColumnsAndUpdateSortingKey(ctx, conn, tableName,
						"DstCountry",
						`Dst1stAS UInt32`,
						`Dst2ndAS UInt32`,
						`Dst3rdAS UInt32`,
					)
				}
				if err != nil {
					return err
				}
				return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s %s`,
					tableName, modifications))
			},
		}
	}
}

func (c *Component) migrationStepAddDstCommunitiesColumn(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
		Args: []interface{}{"flows", "DstCommunities"},
		Do: func() error {
			modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, "flows",
				"Dst3rdAS",
				"DstCommunities Array(UInt32)")
			if err != nil {
				return err
			}
			return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE flows %s`, modifications))
		},
	}
}

func (c *Component) migrationStepAddDstLargeCommunitiesColumn(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
		Args: []interface{}{"flows", "DstLargeCommunities"},
		Do: func() error {
			modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, "flows",
				"DstCommunities",
				"DstLargeCommunities Array(UInt128)")
			if err != nil {
				return err
			}
			return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE flows %s`, modifications))
		},
	}
}

func (c *Component) migrationStepAddSrcNetMaskDstNetMaskColumns(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
		Args: []interface{}{"flows", "SrcNetMask"},
		Do: func() error {
			modifications, err := addColumnsAndUpdateSortingKey(ctx, conn, "flows",
				"DstAddr",
				"SrcNetMask UInt8",
				"DstNetMask UInt8")
			if err != nil {
				return err
			}
			return conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE flows %s`, modifications))
		},
	}
}

func (c *Component) migrationStepAddSrcNetPrefixDstNetPrefixColumn(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.columns
WHERE table = $1 AND database = currentDatabase() AND name = $2`,
		Args: []interface{}{"flows", "SrcNetPrefix"},
		Do: func() error {
			sql := func(prefix string) string {
				return fmt.Sprintf(`
%sNetPrefix String ALIAS
CASE
 WHEN EType = 0x800 THEN concat(replaceRegexpOne(IPv6CIDRToRange(%sAddr, (96 + %sNetMask)::UInt8).1::String, '^::ffff:', ''), '/', %sNetMask::String)
 WHEN EType = 0x86dd THEN concat(IPv6CIDRToRange(%sAddr, %sNetMask).1::String, '/', %sNetMask::String)
 ELSE ''
END
`, prefix, prefix, prefix, prefix, prefix, prefix, prefix)
			}
			return conn.Exec(ctx, fmt.Sprintf("ALTER TABLE flows %s",
				addColumnsAfter("DstNetMask", sql("Src"), sql("Dst"))))
		},
	}
}

func (c *Component) migrationsStepCreateFlowsConsumerTable(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		if resolution.Interval == 0 {
			// Consumer for the flows table are done later.
			return nullMigrationStep
		}
		tableName := fmt.Sprintf("flows_%s", resolution.Interval)
		viewName := fmt.Sprintf("%s_consumer", tableName)
		selectClause := fmt.Sprintf(`
SELECT *
EXCEPT (SrcAddr, DstAddr, SrcNetMask, DstNetMask, SrcPort, DstPort, DstASPath, DstCommunities, DstLargeCommunities)
REPLACE toStartOfInterval(TimeReceived, toIntervalSecond(%d)) AS TimeReceived`,
			uint64(resolution.Interval.Seconds()))
		selectClause = strings.TrimSpace(strings.ReplaceAll(selectClause, "\n", " "))
		return migrationStep{
			CheckQuery: queryTableHash(10874532506016793032,
				fmt.Sprintf("AND as_select LIKE '%s FROM %%'", selectClause)),
			Args: []interface{}{viewName},
			// No GROUP BY, the SummingMergeTree will take care of that
			Do: func() error {
				l.Debug().Msg("drop consumer table")
				err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s SYNC`, viewName))
				if err != nil {
					return fmt.Errorf("cannot drop consumer table: %w", err)
				}
				l.Debug().Msg("create consumer table")
				return conn.Exec(ctx, fmt.Sprintf(`
CREATE MATERIALIZED VIEW %s TO %s
AS %s
FROM %s`, viewName, tableName, selectClause, "flows"))
			},
		}
	}
}

func (c *Component) migrationsStepSetTTLFlowsTable(resolution ResolutionConfiguration) migrationStepFunc {
	return func(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
		if resolution.TTL == 0 {
			l.Info().Msgf("not changing TTL for flows table with interval %s", resolution.Interval)
			return migrationStep{
				CheckQuery: `SELECT 1`,
				Args:       []interface{}{},
				Do:         func() error { return nil },
			}
		}
		tableName := "flows"
		if resolution.Interval != 0 {
			tableName = fmt.Sprintf("flows_%s", resolution.Interval)
		}
		seconds := uint64(resolution.TTL.Seconds())
		ttl := fmt.Sprintf("TTL TimeReceived + toIntervalSecond(%d)", seconds)
		return migrationStep{
			CheckQuery: `
SELECT 1 FROM system.tables
WHERE name = $1 AND database = currentDatabase() AND engine_full LIKE $2`,
			Args: []interface{}{
				tableName,
				fmt.Sprintf("%% %s %%", ttl),
			},
			Do: func() error {
				l.Warn().Msgf("updating TTL of flows table with interval %s, this can take a long time",
					resolution.Interval)
				return conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s MODIFY %s", tableName, ttl))
			},
		}
	}
}

func (c *Component) migrationStepCreateExportersView(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	return migrationStep{
		CheckQuery: queryTableHash(9989732154180416521, ""),
		Args:       []interface{}{"exporters"},
		Do: func() error {
			l.Debug().Msg("drop exporters table")
			err := conn.Exec(ctx, `DROP TABLE IF EXISTS exporters SYNC`)
			if err != nil {
				return fmt.Errorf("cannot drop exporters table: %w", err)
			}
			return conn.Exec(ctx, `
CREATE MATERIALIZED VIEW exporters
ENGINE = ReplacingMergeTree(TimeReceived)
ORDER BY (ExporterAddress, IfName)
AS
SELECT DISTINCT
 TimeReceived,
 ExporterAddress,
 ExporterName,
 ExporterGroup,
 ExporterRole,
 ExporterSite,
 ExporterRegion,
 ExporterTenant,
 [InIfName, OutIfName][num] AS IfName,
 [InIfDescription, OutIfDescription][num] AS IfDescription,
 [InIfSpeed, OutIfSpeed][num] AS IfSpeed,
 [InIfConnectivity, OutIfConnectivity][num] AS IfConnectivity,
 [InIfProvider, OutIfProvider][num] AS IfProvider,
 [InIfBoundary, OutIfBoundary][num] AS IfBoundary
FROM flows
ARRAY JOIN arrayEnumerate([1,2]) AS num
`)
		},
	}
}

func (c *Component) migrationStepCreateProtocolsDictionary(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	protocolsURL := fmt.Sprintf("%s/api/v0/orchestrator/clickhouse/protocols.csv", c.config.OrchestratorURL)
	source := fmt.Sprintf(`SOURCE(HTTP(URL '%s' FORMAT 'CSVWithNames'))`, protocolsURL)
	settings := `SETTINGS(format_csv_allow_single_quotes = 0)`
	sourceLike := fmt.Sprintf("%% %s%% %s%%", source, settings)
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.tables
 WHERE name = $1 AND database = currentDatabase() AND create_table_query LIKE $2`,
		Args: []interface{}{"protocols", sourceLike},
		Do: func() error {
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE OR REPLACE DICTIONARY protocols (
 proto UInt8 INJECTIVE,
 name String,
 description String
)
PRIMARY KEY proto
%s
LIFETIME(MIN 0 MAX 3600)
LAYOUT(HASHED())
%s
`, source, settings))
		},
	}
}

func (c *Component) migrationStepCreateASNsDictionary(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	asnsURL := fmt.Sprintf("%s/api/v0/orchestrator/clickhouse/asns.csv", c.config.OrchestratorURL)
	source := fmt.Sprintf(`SOURCE(HTTP(URL '%s' FORMAT 'CSVWithNames'))`, asnsURL)
	settings := `SETTINGS(format_csv_allow_single_quotes = 0)`
	sourceLike := fmt.Sprintf("%% %s%% %s%%", source, settings)
	return migrationStep{
		CheckQuery: `
SELECT 1 FROM system.tables
WHERE name = $1 AND database = currentDatabase() AND create_table_query LIKE $2`,
		Args: []interface{}{"asns", sourceLike},
		Do: func() error {
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE OR REPLACE DICTIONARY asns (
 asn UInt32 INJECTIVE,
 name String
)

PRIMARY KEY asn
%s
LIFETIME(MIN 0 MAX 3600)
LAYOUT(HASHED())
%s
`, source, settings))
		},
	}

}

func (c *Component) migrationStepCreateNetworksDictionary(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	networksURL := fmt.Sprintf("%s/api/v0/orchestrator/clickhouse/networks.csv", c.config.OrchestratorURL)
	source := fmt.Sprintf(`SOURCE(HTTP(URL '%s' FORMAT 'CSVWithNames'))`, networksURL)
	settings := `SETTINGS(format_csv_allow_single_quotes = 0)`
	sourceLike := fmt.Sprintf("%% %s%% %s%%", source, settings)
	return migrationStep{
		CheckQuery: queryTableHash(5246378884861475308, "AND create_table_query LIKE $2"),
		Args:       []interface{}{"networks", sourceLike},
		Do: func() error {
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE OR REPLACE DICTIONARY networks (
 network String,
 name String,
 role String,
 site String,
 region String,
 tenant String
)

PRIMARY KEY network
%s
LIFETIME(MIN 0 MAX 3600)
LAYOUT(IP_TRIE())
%s
`, source, settings))
		},
	}

}

func (c *Component) migrationStepCreateRawFlowsTable(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	tableName := fmt.Sprintf("flows_%d_raw", flow.CurrentSchemaVersion)
	kafkaEngine := fmt.Sprintf("Kafka SETTINGS %s", strings.Join([]string{
		fmt.Sprintf(`kafka_broker_list = '%s'`,
			strings.Join(c.config.Kafka.Brokers, ",")),
		fmt.Sprintf(`kafka_topic_list = '%s-v%d'`,
			c.config.Kafka.Topic, flow.CurrentSchemaVersion),
		`kafka_group_name = 'clickhouse'`,
		`kafka_format = 'Protobuf'`,
		fmt.Sprintf(`kafka_schema = 'flow-%d.proto:FlowMessagev%d'`,
			flow.CurrentSchemaVersion, flow.CurrentSchemaVersion),
		fmt.Sprintf(`kafka_num_consumers = %d`, c.config.Kafka.Consumers),
		`kafka_thread_per_consumer = 1`,
		`kafka_handle_error_mode = 'stream'`,
	}, ", "))
	return migrationStep{
		CheckQuery: queryTableHash(8163754828379578018, "AND engine_full = $2"),
		Args:       []interface{}{tableName, kafkaEngine},
		Do: func() error {
			l.Debug().Msg("drop raw consumer table")
			err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s_consumer SYNC`, tableName))
			if err != nil {
				return fmt.Errorf("cannot drop raw consumer table: %w", err)
			}
			l.Debug().Msg("drop raw table")
			err = conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s SYNC`, tableName))
			if err != nil {
				return fmt.Errorf("cannot drop raw table: %w", err)
			}
			l.Debug().Msg("create raw table")
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE TABLE %s
(
%s,
DstLargeCommunities Nested(ASN UInt32, LocalData1 UInt32, LocalData2 UInt32)
)
ENGINE = %s`, tableName, partialSchema(
				"SrcNetName", "DstNetName",
				"SrcNetRole", "DstNetRole",
				"SrcNetSite", "DstNetSite",
				"SrcNetRegion", "DstNetRegion",
				"SrcNetTenant", "DstNetTenant",
				"Dst1stAS", "Dst2ndAS", "Dst3rdAS",
				"DstLargeCommunities",
			), kafkaEngine))
		},
	}
}

func (c *Component) migrationStepCreateRawFlowsConsumerView(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	tableName := fmt.Sprintf("flows_%d_raw", flow.CurrentSchemaVersion)
	viewName := fmt.Sprintf("%s_consumer", tableName)
	return migrationStep{
		CheckQuery: queryTableHash(7925127510274634003, "AND as_select LIKE '% WHERE length(_error) = 0'"),
		Args:       []interface{}{viewName},
		Do: func() error {
			l.Debug().Msg("drop consumer table")
			err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s SYNC`, viewName))
			if err != nil {
				return fmt.Errorf("cannot drop consumer table: %w", err)
			}
			l.Debug().Msg("create consumer table")
			largeCommunitiesColumns := strings.Join([]string{
				"`DstLargeCommunities.ASN`",
				"`DstLargeCommunities.LocalData1`",
				"`DstLargeCommunities.LocalData2`"}, ",")
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE MATERIALIZED VIEW %s TO flows
AS WITH arrayCompact(DstASPath) AS c_DstASPath SELECT
 * EXCEPT (%s),
 dictGetOrDefault('networks', 'name', SrcAddr, '') AS SrcNetName,
 dictGetOrDefault('networks', 'name', DstAddr, '') AS DstNetName,
 dictGetOrDefault('networks', 'role', SrcAddr, '') AS SrcNetRole,
 dictGetOrDefault('networks', 'role', DstAddr, '') AS DstNetRole,
 dictGetOrDefault('networks', 'site', SrcAddr, '') AS SrcNetSite,
 dictGetOrDefault('networks', 'site', DstAddr, '') AS DstNetSite,
 dictGetOrDefault('networks', 'region', SrcAddr, '') AS SrcNetRegion,
 dictGetOrDefault('networks', 'region', DstAddr, '') AS DstNetRegion,
 dictGetOrDefault('networks', 'tenant', SrcAddr, '') AS SrcNetTenant,
 dictGetOrDefault('networks', 'tenant', DstAddr, '') AS DstNetTenant,
 c_DstASPath[1] AS Dst1stAS,
 c_DstASPath[2] AS Dst2ndAS,
 c_DstASPath[3] AS Dst3rdAS,
 arrayMap((asn, l1, l2) -> bitShiftLeft(asn::UInt128, 64) + bitShiftLeft(l1::UInt128, 32) + l2::UInt128, %s) AS DstLargeCommunities
FROM %s
WHERE length(_error) = 0`,
				viewName,
				largeCommunitiesColumns, largeCommunitiesColumns,
				tableName))
		},
	}
}

func (c *Component) migrationStepCreateRawFlowsErrorsView(ctx context.Context, l reporter.Logger, conn clickhouse.Conn) migrationStep {
	tableName := fmt.Sprintf("flows_%d_raw", flow.CurrentSchemaVersion)
	viewName := fmt.Sprintf("%s_errors", tableName)
	return migrationStep{
		CheckQuery: queryTableHash(9120662669408051900, ""),
		Args:       []interface{}{viewName},
		Do: func() error {
			l.Debug().Msg("drop kafka errors table")
			err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s SYNC`, viewName))
			if err != nil {
				return fmt.Errorf("cannot drop kafka errors table: %w", err)
			}
			l.Debug().Msg("create kafka errors table")
			return conn.Exec(ctx, fmt.Sprintf(`
CREATE MATERIALIZED VIEW %s
ENGINE = MergeTree
ORDER BY (timestamp, topic, partition, offset)
PARTITION BY toYYYYMMDDhhmmss(toStartOfHour(timestamp))
TTL timestamp + INTERVAL 1 DAY
AS SELECT
 now() AS timestamp,
 _topic AS topic,
 _partition AS partition,
 _offset AS offset,
 _raw_message AS raw,
 _error AS error
FROM %s
WHERE length(_error) > 0`,
				viewName,
				tableName))
		},
	}
}
