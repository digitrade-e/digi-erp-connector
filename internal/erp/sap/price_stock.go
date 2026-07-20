package sap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

var ErrNotImplemented = errors.New("sap price/stock not implemented")

func FetchPriceAndStock(ctx context.Context, dbConn *sql.DB, cfg config.Config, req erp.PriceStockRequest) (erp.PriceStockResult, error) {
	_ = cfg
	if ctx == nil {
		ctx = context.Background()
	}
	if dbConn == nil {
		return erp.PriceStockResult{}, errors.New("db connection is required")
	}

	skus := uniqueStrings(req.SKUList)
	if len(skus) == 0 {
		return erp.PriceStockResult{Items: []erp.PriceStockItem{}}, nil
	}

	cardCode := strings.TrimSpace(req.UserExtID)
	if cardCode == "" {
		return erp.PriceStockResult{}, errors.New("userExtId is required")
	}

	warehouse := firstNonEmpty(req.Warehouses)
	if warehouse == "" {
		return erp.PriceStockResult{}, errors.New("warehouse is required")
	}

	asOfDate := strings.TrimSpace(req.Date)
	if asOfDate == "" {
		asOfDate = time.Now().Format("2006-01-02")
	}

	skuUnion, skuArgs := buildSkuUnion(skus)
	args := []any{
		sql.Named("cardCode", cardCode),
		sql.Named("asOfDate", asOfDate),
		sql.Named("warehouse", warehouse),
	}
	args = append(args, skuArgs...)

	query := fmt.Sprintf(`
WITH SkuList AS (
        %s
),
Cust AS (
    SELECT TOP 1 T5.ListNum
    FROM OCRD AS T5 WITH (NOLOCK)
    WHERE T5.CardCode = @cardCode
),
CustGroup AS (
    SELECT TOP 1 GroupCode
    FROM OCRD WITH (NOLOCK)
    WHERE CardCode = @cardCode
),
BasePrice AS (
    SELECT
        OITM.ItemCode,
        OITM.FirmCode,
        ITM1.Price    AS PriceListPrice,
        ITM1.Currency AS Currency
    FROM OITM WITH (NOLOCK)
    INNER JOIN ITM1 WITH (NOLOCK)
        ON OITM.ItemCode = ITM1.ItemCode
    CROSS JOIN Cust
    WHERE ITM1.PriceList = Cust.ListNum
      AND OITM.ItemCode IN (SELECT sku FROM SkuList)
),
SpecialPrice AS (
    SELECT
        P.ItemCode,
        P.ListNum,
        P.Price    AS OSPPPrice,
        P.Discount AS OSPPDiscount
    FROM OSPP AS P WITH (NOLOCK)
    WHERE P.CardCode = @cardCode
      AND P.Valid = 'Y'
      AND (
            (P.ValidFrom IS NULL OR P.ValidFrom <= @asOfDate)
        AND (P.ValidTo   IS NULL OR P.ValidTo   >= @asOfDate)
      )
),
AllDiscountRules AS (
    SELECT
        BP.ItemCode,
        E.Type                   AS RuleType,
        E1.ObjType,
        E1.ObjKey,
        E1.Discount              AS DiscountPct,
        CASE E1.ObjType
            WHEN '4'  THEN N'Discount group (item)'
            WHEN '43' THEN N'Discount group (manufacturer)'
            WHEN '52' THEN N'Discount group (item group)'
        END                      AS RuleSource
    FROM BasePrice AS BP
    CROSS JOIN CustGroup
    INNER JOIN OEDG AS E WITH (NOLOCK)
        ON (
             (E.Type = 'S' AND E.ObjCode = @cardCode)
          OR (E.Type = 'C' AND E.ObjCode = CONVERT(NVARCHAR, CustGroup.GroupCode))
          OR (E.ObjType = '-1' AND E.ObjCode = '0')
          )
       AND (
            E.ValidFor = 'N'
            OR (
                E.ValidFor = 'Y'
                AND (
                        (E.ValidForm IS NULL OR E.ValidForm <= @asOfDate)
                    AND (E.ValidTo   IS NULL OR E.ValidTo   >= @asOfDate)
                )
            )
       )
    INNER JOIN EDG1 AS E1 WITH (NOLOCK)
        ON E1.AbsEntry = E.AbsEntry
       AND E1.ObjType IN ('4','43','52')
    LEFT JOIN OITM WITH (NOLOCK)
        ON OITM.ItemCode = BP.ItemCode
    WHERE
          (E1.ObjType = '4'  AND E1.ObjKey = BP.ItemCode)
       OR (E1.ObjType = '43' AND TRY_CAST(E1.ObjKey AS INT) = BP.FirmCode)
       OR (E1.ObjType = '52' AND TRY_CAST(E1.ObjKey AS INT) = OITM.ItmsGrpCod)
),
BpDiscountMode AS (
    SELECT
        ISNULL(NULLIF(C.DiscRel, ''), 'H') AS DiscountMode
    FROM OCRD AS C WITH (NOLOCK)
    WHERE C.CardCode = @cardCode
),
BestDiscountPerItem AS (
    SELECT
        R.ItemCode,
        Mode.DiscountMode,
        CASE Mode.DiscountMode
            WHEN 'H' THEN MAX(R.DiscountPct)
            WHEN 'L' THEN MIN(R.DiscountPct)
            WHEN 'A' THEN AVG(R.DiscountPct)
            WHEN 'S' THEN
                CASE
                    WHEN SUM(R.DiscountPct) > 100.0 THEN 100.0
                    ELSE SUM(R.DiscountPct)
                END
            WHEN 'M' THEN
                CASE
                    WHEN MAX(CASE WHEN R.DiscountPct >= 100 THEN 1 ELSE 0 END) = 1
                        THEN 100.0
                    ELSE
                        100.0 * (
                            1.0 - EXP(
                                SUM(
                                    CASE
                                        WHEN R.DiscountPct IS NULL OR R.DiscountPct >= 100
                                            THEN 0.0
                                        ELSE LOG((100.0 - R.DiscountPct) / 100.0)
                                    END
                                )
                            )
                        )
                END
            ELSE MAX(R.DiscountPct)
        END AS DiscountPct
    FROM AllDiscountRules AS R
    CROSS JOIN BpDiscountMode AS Mode
    GROUP BY R.ItemCode, Mode.DiscountMode
),
DiscountRuleType AS (
    SELECT
        R.ItemCode,
        R.RuleType,
        ROW_NUMBER() OVER (
            PARTITION BY R.ItemCode
            ORDER BY
                CASE
                    WHEN Mode.DiscountMode = 'L' THEN R.DiscountPct
                    ELSE -R.DiscountPct
                END ASC,
                R.RuleType
        ) AS rn
    FROM AllDiscountRules AS R
    CROSS JOIN BpDiscountMode AS Mode
),
PromoDiscount AS (
    SELECT
        I.ItemCode,
        E.Type AS PromoType,
        E1.Discount AS PromoDiscount
    FROM OEDG AS E WITH (NOLOCK)
    INNER JOIN EDG1 AS E1 WITH (NOLOCK)
        ON E1.AbsEntry = E.AbsEntry
       AND E1.ObjType = '4'
    INNER JOIN OITM AS I WITH (NOLOCK)
        ON I.ItemCode = E1.ObjKey
    WHERE E.Type = 'A'
      AND (
            E.ValidFor = 'N'
         OR (
                E.ValidFor = 'Y'
            AND (
                    (E.ValidForm IS NULL OR E.ValidForm <= @asOfDate)
                AND (E.ValidTo   IS NULL OR E.ValidTo   >= @asOfDate)
            )
         )
      )
),
TreeParents AS (
    SELECT S.sku AS ParentCode
    FROM SkuList AS S
    INNER JOIN OITT AS H WITH (NOLOCK)
        ON H.Code = S.sku
       AND H.TreeType = 'S'
),
ParentChildren AS (
    SELECT H.Code AS ParentCode,
           L.Code AS ChildCode
    FROM OITT AS H WITH (NOLOCK)
    INNER JOIN ITT1 AS L WITH (NOLOCK)
        ON L.Father = H.Code
    WHERE H.TreeType = 'S'
      AND H.Code IN (SELECT sku FROM SkuList)
),
AllItemsForStock AS (
    SELECT S.sku AS ParentCode,
           S.sku AS ItemCodeToCheck
    FROM SkuList AS S
    WHERE S.sku NOT IN (SELECT ParentCode FROM TreeParents)

    UNION ALL

    SELECT C.ParentCode,
           C.ChildCode AS ItemCodeToCheck
    FROM ParentChildren AS C
),
StockRaw AS (
    SELECT
        W.ItemCode,
        W.WhsCode,
        W.OnHand,
        W.OnOrder,
        W.IsCommited
    FROM OITW AS W WITH (NOLOCK)
    WHERE W.WhsCode = @warehouse
      AND W.ItemCode IN (SELECT ItemCodeToCheck FROM AllItemsForStock)
),
StockPerParentRows AS (
    SELECT
        A.ParentCode,
        S.ItemCode,
        S.WhsCode,
        S.OnHand,
        S.OnOrder,
        S.IsCommited,
        ROW_NUMBER() OVER (
            PARTITION BY A.ParentCode
            ORDER BY
                CASE WHEN S.OnHand IS NULL THEN 1 ELSE 0 END,
                S.OnHand ASC
        ) AS rn
    FROM AllItemsForStock AS A
    LEFT JOIN StockRaw AS S
      ON S.ItemCode = A.ItemCodeToCheck
),
Stock AS (
    SELECT
        SPR.ParentCode AS ItemCode,
        SPR.WhsCode    AS warehouseCode,
        SPR.OnHand     AS stock,
        SPR.OnOrder    AS onOrder,
        SPR.IsCommited AS commited
    FROM StockPerParentRows AS SPR
    WHERE SPR.rn = 1
)
SELECT
    BP.ItemCode                                                      AS sku,
    @cardCode                                                        AS CardCode,
    CAST((SELECT ListNum FROM Cust) AS DECIMAL(19,4))                AS PriceList,
    BP.Currency,
    CAST(BP.PriceListPrice AS DECIMAL(19,4))                         AS PriceListPrice,
    CAST(SP.OSPPPrice AS DECIMAL(19,4))                              AS OSPPPrice,
    CAST(SP.OSPPDiscount AS DECIMAL(19,4))                           AS OSPPDiscount,
    CAST(BD.DiscountPct AS DECIMAL(19,4))                            AS BPGroupDiscount,
    CAST(BD.DiscountMode AS NVARCHAR(1))                             AS BPGroupDiscountType,
    CAST(
        CASE
            WHEN SP.OSPPPrice IS NOT NULL AND SP.OSPPPrice > 0 THEN NULL
            WHEN SP.OSPPDiscount IS NOT NULL THEN NULL
            WHEN BD.DiscountPct IS NOT NULL THEN DRT.RuleType
            WHEN PD.PromoDiscount IS NOT NULL THEN PD.PromoType
            ELSE NULL
        END
        AS NVARCHAR(1)
    )                                                                AS OedgType,
    CAST(NULL AS NVARCHAR(255))                                      AS ManufacturerName,
    CAST(NULL AS DECIMAL(19,4))                                      AS ManufacturerDiscount,
    CAST(PD.PromoDiscount AS DECIMAL(19,4))                          AS PromoDiscount,
    ISNULL(S.warehouseCode, '')                                      AS warehouseCode,
    CAST(S.stock AS DECIMAL(19,4))                                   AS stock,
    CAST(S.onOrder AS DECIMAL(19,4))                                 AS onOrder,
    CAST(S.commited AS DECIMAL(19,4))                                AS commited,
    CASE
        WHEN SP.OSPPPrice IS NOT NULL AND SP.OSPPPrice > 0 THEN N'OSPP explicit price'
        WHEN SP.OSPPDiscount IS NOT NULL THEN N'OSPP discount'
        WHEN BD.DiscountPct IS NOT NULL THEN
            CASE BD.DiscountMode
                WHEN 'H' THEN N'Discount groups (highest)'
                WHEN 'L' THEN N'Discount groups (lowest)'
                WHEN 'A' THEN N'Discount groups (average)'
                WHEN 'S' THEN N'Discount groups (sum)'
                WHEN 'M' THEN N'Discount groups (mixed)'
                ELSE N'Discount groups'
            END
        WHEN PD.PromoDiscount IS NOT NULL THEN N'Promo (EDG Type A)'
        ELSE N'Base price list'
    END                                                              AS PriceSource,
    CAST(
        CASE
            WHEN SP.OSPPPrice IS NOT NULL AND SP.OSPPPrice > 0
                THEN SP.OSPPPrice
            WHEN SP.OSPPDiscount IS NOT NULL
                THEN BP.PriceListPrice * (100.0 - SP.OSPPDiscount) / 100.0
            WHEN BD.DiscountPct IS NOT NULL
                THEN BP.PriceListPrice * (100.0 - BD.DiscountPct) / 100.0
            WHEN PD.PromoDiscount IS NOT NULL
                THEN BP.PriceListPrice * (100.0 - PD.PromoDiscount) / 100.0
            ELSE BP.PriceListPrice
        END
        AS DECIMAL(19,4)
    )                                                                AS FinalPrice
FROM BasePrice AS BP
LEFT JOIN SpecialPrice AS SP
       ON SP.ItemCode = BP.ItemCode
      AND (SP.ListNum IS NULL OR SP.ListNum = (SELECT ListNum FROM Cust))
LEFT JOIN BestDiscountPerItem AS BD
       ON BD.ItemCode = BP.ItemCode
LEFT JOIN DiscountRuleType AS DRT
       ON DRT.ItemCode = BP.ItemCode
      AND DRT.rn = 1
LEFT JOIN PromoDiscount AS PD
       ON PD.ItemCode = BP.ItemCode
LEFT JOIN Stock AS S
       ON S.ItemCode = BP.ItemCode
ORDER BY BP.ItemCode
OPTION (RECOMPILE);
`, skuUnion)

	rows, err := dbConn.QueryContext(ctx, query, args...)
	if err != nil {
		return erp.PriceStockResult{}, err
	}
	defer rows.Close()

	items := make([]erp.PriceStockItem, 0)
	for rows.Next() {
		var (
			sku                  string
			cardCodeVal          sql.NullString
			priceList            sql.NullFloat64
			currency             sql.NullString
			priceListPrice       sql.NullFloat64
			osppPrice            sql.NullFloat64
			osppDiscount         sql.NullFloat64
			bpGroupDiscount      sql.NullFloat64
			bpGroupDiscountType  sql.NullString
			oedgType             sql.NullString
			manufacturerName     sql.NullString
			manufacturerDiscount sql.NullFloat64
			promoDiscount        sql.NullFloat64
			warehouseCode        sql.NullString
			stock                sql.NullFloat64
			onOrder              sql.NullFloat64
			commited             sql.NullFloat64
			priceSource          sql.NullString
			finalPrice           sql.NullFloat64
		)

		if err := rows.Scan(
			&sku,
			&cardCodeVal,
			&priceList,
			&currency,
			&priceListPrice,
			&osppPrice,
			&osppDiscount,
			&bpGroupDiscount,
			&bpGroupDiscountType,
			&oedgType,
			&manufacturerName,
			&manufacturerDiscount,
			&promoDiscount,
			&warehouseCode,
			&stock,
			&onOrder,
			&commited,
			&priceSource,
			&finalPrice,
		); err != nil {
			return erp.PriceStockResult{}, err
		}

		prices := map[string]float64{}
		addFloat(prices, "priceListPrice", priceListPrice)
		addFloat(prices, "osppPrice", osppPrice)
		addFloat(prices, "osppDiscount", osppDiscount)
		addFloat(prices, "bpGroupDiscount", bpGroupDiscount)
		addFloat(prices, "promoDiscount", promoDiscount)
		addFloat(prices, "manufacturerDiscount", manufacturerDiscount)
		addFloat(prices, "finalPrice", finalPrice)

		wh := strings.TrimSpace(warehouseCode.String)
		if wh == "" {
			wh = warehouse
		}
		stockByWarehouse := map[string]float64{}
		if stock.Valid {
			stockByWarehouse[wh] = stock.Float64
		}

		details := map[string]any{
			"cardCode":            cardCodeVal.String,
			"priceList":           nullFloat(priceList),
			"currency":            currency.String,
			"priceSource":         priceSource.String,
			"warehouseCode":       wh,
			"osppDiscount":        nullFloat(osppDiscount),
			"bpGroupDiscountType": bpGroupDiscountType.String,
			"oedgType":            oedgType.String,
			"manufacturerName":    manufacturerName.String,
			"onOrder":             nullFloat(onOrder),
			"commited":            nullFloat(commited),
		}

		items = append(items, erp.PriceStockItem{
			SKU:              sku,
			Prices:           prices,
			StockByWarehouse: stockByWarehouse,
			Details:          details,
		})
	}

	if err := rows.Err(); err != nil {
		return erp.PriceStockResult{}, err
	}

	return erp.PriceStockResult{Items: items}, nil
}

func buildSkuUnion(skus []string) (string, []any) {
	parts := make([]string, 0, len(skus))
	args := make([]any, 0, len(skus))
	for i, sku := range skus {
		name := fmt.Sprintf("sku%d", i)
		if i == 0 {
			parts = append(parts, fmt.Sprintf("SELECT @%s AS sku", name))
		} else {
			parts = append(parts, fmt.Sprintf("UNION ALL SELECT @%s", name))
		}
		args = append(args, sql.Named(name, sku))
	}
	return strings.Join(parts, "\n        "), args
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values []string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func addFloat(out map[string]float64, key string, val sql.NullFloat64) {
	if val.Valid {
		out[key] = val.Float64
	}
}

func nullFloat(v sql.NullFloat64) any {
	if v.Valid {
		return v.Float64
	}
	return nil
}
