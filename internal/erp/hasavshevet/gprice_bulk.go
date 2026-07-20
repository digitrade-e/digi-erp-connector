package hasavshevet

import (
	"context"
	"database/sql"
	"errors"
)

const gpriceBulkProcName = "dbo.GPRICE_Bulk"

const gpriceBulkProcedureSQL = `
CREATE OR ALTER PROCEDURE dbo.GPRICE_Bulk
(
	@CustomerId   varchar(15),
	@SkusJson     nvarchar(max),
	@DocumentID   int = 1,
	@DatF         datetime = NULL,
	@Quantity     float = 1,
	@Assign       varchar(15) = '1',
	@Split        tinyint = 0
)
AS
BEGIN
	SET NOCOUNT ON;

	IF @DatF IS NULL
		SET @DatF = GETDATE();

	DECLARE @StationId varchar(30) =
		'API_' + REPLACE(CONVERT(varchar(36), NEWID()), '-', '');

	DECLARE @res tinyint;

	BEGIN TRY
		CREATE TABLE #SkuList (RowNum int IDENTITY(1,1) PRIMARY KEY, Sku varchar(20) NOT NULL);

		INSERT INTO #SkuList (Sku)
		SELECT CAST([value] AS varchar(20))
		FROM OPENJSON(@SkusJson);

		IF NOT EXISTS (SELECT 1 FROM #SkuList)
		BEGIN
			SELECT
				CAST(NULL AS varchar(20)) AS sku,
				CAST(NULL AS float)       AS price,
				CAST(NULL AS varchar(5))  AS currency,
				CAST(NULL AS float)       AS discountPrc,
				CAST(NULL AS float)       AS commisionPrc,
				CAST(NULL AS int)         AS gpFlag,
				CAST(NULL AS datetime)    AS [date],
				CAST(NULL AS int)         AS documentId
			WHERE 1 = 0;

			RETURN 0;
		END

		INSERT INTO GETPRICE
		(
			StationID, AccountKey, ItemKey,
			Quantity, DatF, DocumentID,
			Price, DiscountPrc, CommisionPrc
		)
		SELECT
			@StationId, @CustomerId, s.Sku,
			@Quantity, @DatF, @DocumentID,
			0, 0, 0
		FROM #SkuList s;

		EXEC dbo.GPRICE
			@STATIONID = @StationId,
			@ASSIGN    = @Assign,
			@Split     = @Split,
			@RESULT    = @res OUTPUT;

		CREATE TABLE #Out
		(
			RowNum int NOT NULL,
			sku varchar(20) NOT NULL,
			price float NULL,
			currency varchar(5) NULL,
			discountPrc float NULL,
			commisionPrc float NULL,
			gpFlag int NULL,
			[date] datetime NULL,
			documentId int NULL
		);

		INSERT INTO #Out
		SELECT
			s.RowNum,
			s.Sku,
			gp.Price,
			gp.Coin,
			gp.DiscountPrc,
			gp.CommisionPrc,
			gp.GPFlag,
			gp.DatF,
			gp.DocumentID
		FROM #SkuList s
		LEFT JOIN GETPRICE gp
		  ON gp.StationID  = @StationId
		 AND gp.ItemKey    = s.Sku
		 AND gp.AccountKey = @CustomerId;

		DELETE FROM GETPRICE
		WHERE StationID = @StationId;

		SELECT
			sku, price, currency, discountPrc, commisionPrc, gpFlag, [date], documentId
		FROM #Out
		ORDER BY RowNum;

		RETURN 0;
	END TRY
	BEGIN CATCH
		DELETE FROM GETPRICE
		WHERE StationID = @StationId;

		DECLARE @Err nvarchar(4000) = ERROR_MESSAGE();
		RAISERROR('GPRICE_Bulk failed: %s', 16, 1, @Err);
		RETURN 1;
	END CATCH
END
`

func EnsureGPriceBulkProcedure(ctx context.Context, dbConn *sql.DB) (bool, error) {
	if dbConn == nil {
		return false, errors.New("db connection is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	exists, err := procedureExists(ctx, dbConn, gpriceBulkProcName)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	if _, err := dbConn.ExecContext(ctx, gpriceBulkProcedureSQL); err != nil {
		return false, err
	}
	return true, nil
}

func procedureExists(ctx context.Context, dbConn *sql.DB, name string) (bool, error) {
	if dbConn == nil {
		return false, errors.New("db connection is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	const query = `
		SELECT 1
		WHERE EXISTS (
			SELECT 1
			FROM sys.objects
			WHERE object_id = OBJECT_ID(@procName)
			  AND type IN (N'P', N'PC')
		);
	`

	var found int
	err := dbConn.QueryRowContext(ctx, query, sql.Named("procName", name)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
