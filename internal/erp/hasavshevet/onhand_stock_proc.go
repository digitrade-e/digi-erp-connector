package hasavshevet

import (
	"context"
	"database/sql"
	"errors"
)

const onHandStockProcName = "dbo.GetOnHandStockForSkus"

const onHandStockProcedureSQL = `
CREATE OR ALTER PROCEDURE dbo.GetOnHandStockForSkus
(
	@Warehouse varchar(30),
	@SkusJson nvarchar(max)
)
AS
BEGIN
	SET NOCOUNT ON;

	CREATE TABLE #SkuList
	(
		RowNum int IDENTITY(1,1) PRIMARY KEY,
		ItemKey varchar(50) NOT NULL
	);

	INSERT INTO #SkuList (ItemKey)
	SELECT CAST([value] AS varchar(50))
	FROM OPENJSON(@SkusJson);

	IF NOT EXISTS (SELECT 1 FROM #SkuList)
	BEGIN
		SELECT
			CAST(NULL AS varchar(50)) AS makat,
			CAST(NULL AS float) AS warehouseStock,
			CAST(NULL AS float) AS openOrdersStock,
			CAST(NULL AS float) AS totalStock
		WHERE 1 = 0;

		RETURN 0;
	END

	;WITH Bal AS
	(
		SELECT
			v.ITEMKEY,
			SUM(ISNULL(v.ITEMWARHBAL, 0)) AS warehouseStock
		FROM dbo.vBalItemWarehouse v
		INNER JOIN #SkuList s
			ON s.ItemKey = v.ITEMKEY
		WHERE v.WAREHOUSE = @Warehouse
		GROUP BY v.ITEMKEY
	),
	FilteredStock AS
	(
		SELECT s1.ID
		FROM dbo.Stock AS s1
		WHERE s1.DocumentID = 11
		  AND s1.DocNumber NOT LIKE '90%'
		  AND s1.Status = 1
		  AND s1.WareHouse = @Warehouse
	),
	OpenOrders AS
	(
		SELECT
			sm.ItemKey,
			SUM(sm.SupplyQuantity) AS openOrdersStock
		FROM FilteredStock fs
		INNER JOIN dbo.StockMoves sm
			ON sm.StockId = fs.ID
		INNER JOIN #SkuList s
			ON s.ItemKey = sm.ItemKey
		WHERE sm.SupplyQuantity <> 0
		  AND sm.Status <> 2
		GROUP BY sm.ItemKey
	)
	SELECT
		s.ItemKey AS makat,
		CAST(ISNULL(b.warehouseStock, 0) AS float) AS warehouseStock,
		CAST(ISNULL(o.openOrdersStock, 0) AS float) AS openOrdersStock,
		CAST(ISNULL(b.warehouseStock, 0) - ISNULL(o.openOrdersStock, 0) AS float) AS totalStock
	FROM #SkuList s
	LEFT JOIN Bal b
		ON b.ITEMKEY = s.ItemKey
	LEFT JOIN OpenOrders o
		ON o.ItemKey = s.ItemKey
	ORDER BY s.RowNum;

	RETURN 0;
END
`

func EnsureOnHandStockForSkusProcedure(ctx context.Context, dbConn *sql.DB) (bool, error) {
	if dbConn == nil {
		return false, errors.New("db connection is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	exists, err := procedureExists(ctx, dbConn, onHandStockProcName)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	if _, err := dbConn.ExecContext(ctx, onHandStockProcedureSQL); err != nil {
		return false, err
	}
	return true, nil
}
