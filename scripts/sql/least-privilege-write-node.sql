/*
    digi-erp-connector — least-privilege login for a WRITE (order) node
    ==================================================================

    A write node only builds IMOVEIN order files. It reads exactly two tables:

        [dbo].[Accounts]   the customer details for the IMOVEIN header
        [dbo].[Rates]      the currency rate (optional — defaults to 1.0)

    That is all. No writes, no other tables, no procedures, no DDL.

    Before running this, consider whether the node needs a database at all:
    if the backend sends an "account" object with each order, the connector
    needs no database whatsoever — leave the db block out of config.yaml and
    skip this script. See docs/deployment-topologies.md.

    Run as an administrator on the SQL Server instance the write node will reach.
*/

USE [master];
GO

IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'erp_connector_orders')
BEGIN
    CREATE LOGIN [erp_connector_orders]
        WITH PASSWORD = N'<CHANGE-ME-strong-password>',
             CHECK_POLICY = ON;
END
GO

/* Change BFL to the customer's database. */
USE [BFL];
GO

IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'erp_connector_orders')
BEGIN
    CREATE USER [erp_connector_orders] FOR LOGIN [erp_connector_orders];
END
GO

/* ---------------------------------------------------------------------------
   Exactly the two tables the order pipeline reads, and only SELECT.

   Column-level grants are possible and even tighter, but the connector selects
   AccountKey, FullName, Address, City, Phone, Agent and HProtect — effectively
   the whole row — so table-level SELECT is honest here.
   --------------------------------------------------------------------------*/
GRANT SELECT ON [dbo].[Accounts] TO [erp_connector_orders];
GRANT SELECT ON [dbo].[Rates]    TO [erp_connector_orders];
GO

/* No db_datareader: this login must not be able to read anything else,
   including price lists, documents or customer balances. */

DENY INSERT, UPDATE, DELETE, ALTER, CREATE TABLE TO [erp_connector_orders];
GO

/* ---------------------------------------------------------------------------
   Verify: the two reads work, everything else does not.
   --------------------------------------------------------------------------*/
EXECUTE AS USER = N'erp_connector_orders';

    SELECT TOP 1 'accounts readable' AS check_result FROM [dbo].[Accounts];
    SELECT TOP 1 'rates readable'    AS check_result FROM [dbo].[Rates];

    /* Each of these should fail with "permission denied" — that is the point: */
    -- SELECT TOP 1 * FROM [dbo].[Items];
    -- UPDATE [dbo].[Accounts] SET City = City;

REVERT;
GO

SELECT
    dp.permission_name,
    dp.state_desc,
    ISNULL(OBJECT_NAME(dp.major_id), '(database)') AS object_name
FROM sys.database_permissions dp
JOIN sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
WHERE pr.name = N'erp_connector_orders'
ORDER BY object_name, dp.permission_name;
GO
