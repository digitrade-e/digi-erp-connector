/*
    digi-erp-connector — least-privilege login for a READ node
    ==========================================================

    Replaces `sa`. A read node runs saved queries and price/stock; it never
    writes. Today the connector logs in as `sa`, which means a stolen bearer
    token is full database administrator — this contains that blast radius.

    Run as an administrator on the SQL Server instance, once per customer
    database. Review every GRANT: this script cannot know which tables a given
    customer's saved queries touch.

    Afterwards, in the connector GUI: set DB user to erp_connector_read, enter
    the password, Save, Test connection. Then restart the service.
*/

USE [master];
GO

/* ---------------------------------------------------------------------------
   1. The login. Replace the password before running — and do not reuse the sa
      password.
   --------------------------------------------------------------------------*/
IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'erp_connector_read')
BEGIN
    CREATE LOGIN [erp_connector_read]
        WITH PASSWORD = N'<CHANGE-ME-strong-password>',
             CHECK_POLICY = ON;
END
GO

/* No server-level roles. The connector never needs sysadmin, securityadmin,
   dbcreator or anything else at instance scope. */

/* ---------------------------------------------------------------------------
   2. Database user. Change BFL to the customer's database.
   --------------------------------------------------------------------------*/
USE [BFL];
GO

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'erp_connector_read')
BEGIN
    CREATE USER [erp_connector_read] FOR LOGIN [erp_connector_read];
END
GO

/* ---------------------------------------------------------------------------
   3. Read access.

   db_datareader grants SELECT on every table. It is the pragmatic choice when
   saved queries are added over time without a schema change going through you —
   it is still enormously better than sa, because it cannot write, alter, drop
   or read other databases.

   If you know exactly which tables are needed, delete this line and grant them
   individually instead (section 3b).
   --------------------------------------------------------------------------*/
ALTER ROLE [db_datareader] ADD MEMBER [erp_connector_read];
GO

/* 3b. Tighter alternative — comment out 3 above and list the tables:

GRANT SELECT ON [dbo].[Items]        TO [erp_connector_read];
GRANT SELECT ON [dbo].[Accounts]     TO [erp_connector_read];
GRANT SELECT ON [dbo].[ExtraNotes]   TO [erp_connector_read];
-- ... one line per table the saved queries read
*/

/* ---------------------------------------------------------------------------
   4. Price/stock procedures.

   The connector calls these; it does not need to read their underlying tables
   directly, because EXECUTE runs with the procedure's own permissions.
   --------------------------------------------------------------------------*/
IF OBJECT_ID(N'dbo.GPRICE_Bulk', N'P') IS NOT NULL
    GRANT EXECUTE ON [dbo].[GPRICE_Bulk] TO [erp_connector_read];
GO
IF OBJECT_ID(N'dbo.GPRICE_BulkJson', N'P') IS NOT NULL
    GRANT EXECUTE ON [dbo].[GPRICE_BulkJson] TO [erp_connector_read];
GO
IF OBJECT_ID(N'dbo.GetOnHandStockForSkus', N'P') IS NOT NULL
    GRANT EXECUTE ON [dbo].[GetOnHandStockForSkus] TO [erp_connector_read];
GO

/* ---------------------------------------------------------------------------
   5. Explicitly deny writes.

   Belt and braces: db_datareader grants no write permission, but an explicit
   DENY survives someone later adding this user to a broader role by mistake.
   Remove this section only if a customer's saved queries genuinely must write —
   saved queries are allowed to, by design, so check before assuming.
   --------------------------------------------------------------------------*/
DENY INSERT, UPDATE, DELETE, ALTER, CREATE TABLE TO [erp_connector_read];
GO

/* ---------------------------------------------------------------------------
   6. DDL is NOT granted.

   The connector's GUI installs GPRICE_Bulk / GetOnHandStockForSkus with
   CREATE OR ALTER when you save Hasavshevet settings. With this login that step
   fails and the GUI shows a note instead — by design, since v1.0.6 saves
   anyway. Install or update those procedures as an administrator instead.
   --------------------------------------------------------------------------*/

/* ---------------------------------------------------------------------------
   7. Verify.
   --------------------------------------------------------------------------*/
EXECUTE AS USER = N'erp_connector_read';

    SELECT TOP 1 'read works' AS check_result FROM [dbo].[Items];

    /* Should fail with "permission denied": */
    -- UPDATE [dbo].[Items] SET Price = Price;

REVERT;
GO

SELECT
    dp.permission_name,
    dp.state_desc,
    ISNULL(OBJECT_NAME(dp.major_id), '(database)') AS object_name
FROM sys.database_permissions dp
JOIN sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
WHERE pr.name = N'erp_connector_read'
ORDER BY object_name, dp.permission_name;
GO
