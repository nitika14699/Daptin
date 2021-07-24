package server

import (
	"github.com/artpar/api2go"
	"github.com/artpar/go.uuid"
	"github.com/artpar/ydb"
	"github.com/buraksezer/olric"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/database"
	"github.com/daptin/daptin/server/resource"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"strings"
)

func CheckSystemSecrets(store *resource.ConfigStore) error {
	jwtSecret, err := store.GetConfigValueFor("jwt.secret", "backend")
	if err != nil {
		u, _ := uuid.NewV4()
		jwtSecret = u.String()
		err = store.SetConfigValueFor("jwt.secret", jwtSecret, "backend")
		resource.CheckErr(err, "Failed to store jwt secret")
	}

	encryptionSecret, err := store.GetConfigValueFor("encryption.secret", "backend")

	if err != nil || len(encryptionSecret) < 10 {
		u, _ := uuid.NewV4()
		newSecret := strings.Replace(u.String(), "-", "", -1)
		err = store.SetConfigValueFor("encryption.secret", newSecret, "backend")
	}
	return err

}

func AddResourcesToApi2Go(api *api2go.API, tables []resource.TableInfo, db database.DatabaseConnection,
	ms *resource.MiddlewareSet, configStore *resource.ConfigStore, olricDb *olric.Olric,
	cruds map[string]*resource.DbResource) {
	for _, table := range tables {

		if table.TableName == "" {
			log.Errorf("Table name is empty, not adding to JSON API, as it will create conflict: %v", table)
			continue
		}

		model := api2go.NewApi2GoModel(table.TableName, table.Columns, int64(table.DefaultPermission), table.Relations)

		res := resource.NewDbResource(model, db, ms, cruds, configStore, olricDb, table)

		cruds[table.TableName] = res

		//if table.IsJoinTable {
		//	we do expose join table as web api
		//continue
		//}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("Recovered in adding routes for table [%v]", table.TableName)
					log.Errorf("Error was: %v", r)
				}
			}()
			api.AddResource(model, res)
		}()
	}

}

func GetTablesFromWorld(db database.DatabaseConnection) ([]resource.TableInfo, error) {

	ts := make([]resource.TableInfo, 0)

	sql, args, err := statementbuilder.Squirrel.
		Select("table_name", "permission", "default_permission",
			"world_schema_json", "is_top_level", "is_hidden", "is_state_tracking_enabled", "default_order",
		).
		From("world").
		Where(goqu.Ex{
			"table_name": goqu.Op{
				"notlike": "%_has_%",
			},
		}).
		Where(goqu.Ex{
			"table_name": goqu.Op{
				"notlike": "%_audit",
			},
		}).
		Where(goqu.Ex{
			"table_name": goqu.Op{
				"notin": []string{"world", "action", "usergroup"},
			},
		}).
		ToSQL()
	if err != nil {
		return nil, err
	}


	stmt1, err := db.Preparex(sql)
	if err != nil {
		log.Errorf("[106] failed to prepare statment: %v", err)
		return nil, err
	}

	defer func(stmt1 *sqlx.Stmt) {
		err := stmt1.Close()
		if err != nil {
			log.Errorf("failed to close prepared statement: %v", err)
		}
	}(stmt1)



	res, err := stmt1.Queryx(args...)
	if err != nil {
		log.Printf("Failed to select from world table: %v", err)
		return ts, err
	}
	defer func() {
		err = res.Close()
		resource.CheckErr(err, "Failed to close db result")
	}()

	for res.Next() {
		var table_name string
		var permission int64
		var default_permission int64
		var world_schema_json string
		var default_order *string
		var is_top_level bool
		var is_hidden bool
		var is_state_tracking_enabled bool

		err = res.Scan(&table_name, &permission, &default_permission, &world_schema_json, &is_top_level, &is_hidden, &is_state_tracking_enabled, &default_order)
		if err != nil {
			log.Errorf("Failed to scan json schema from world: %v", err)
			continue
		}

		var t resource.TableInfo

		err = json.Unmarshal([]byte(world_schema_json), &t)

		for i, col := range t.Columns {
			if col.Name == "" && col.ColumnName != "" {
				col.Name = col.ColumnName
			} else if col.Name != "" && col.ColumnName == "" {
				col.ColumnName = col.Name
			} else if col.Name == "" && col.ColumnName == "" {
				log.Printf("Error, column without name in existing tables: %v", t)
			}
			t.Columns[i] = col
		}

		if err != nil {
			log.Errorf("Failed to unmarshal json schema: %v", err)
			continue
		}

		t.TableName = table_name
		t.Permission = auth.AuthPermission(permission)
		t.DefaultPermission = auth.AuthPermission(default_permission)
		t.IsHidden = is_hidden
		t.IsTopLevel = is_top_level
		t.IsStateTrackingEnabled = is_state_tracking_enabled
		if default_order != nil {
			t.DefaultOrder = *default_order
		}
		ts = append(ts, t)

	}

	log.Printf("Loaded %d tables from world table", len(ts))

	return ts, nil

}

func BuildMiddlewareSet(cmsConfig *resource.CmsConfig,
	cruds *map[string]*resource.DbResource,
	documentProvider ydb.DocumentProvider,
	dtopicMap *map[string]*olric.DTopic) resource.MiddlewareSet {

	var ms resource.MiddlewareSet

	exchangeMiddleware := resource.NewExchangeMiddleware(cmsConfig, cruds)

	tablePermissionChecker := &resource.TableAccessPermissionChecker{}
	objectPermissionChecker := &resource.ObjectAccessPermissionChecker{}
	dataValidationMiddleware := resource.NewDataValidationMiddleware(cmsConfig, cruds)

	createEventHandler := resource.NewCreateEventHandler(cruds, dtopicMap)
	updateEventHandler := resource.NewUpdateEventHandler(cruds, dtopicMap)
	deleteEventHandler := resource.NewDeleteEventHandler(cruds, dtopicMap)

	yhsHandler := resource.NewYJSHandlerMiddleware(documentProvider)

	ms.BeforeFindAll = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		exchangeMiddleware,
		objectPermissionChecker,
	}

	ms.AfterFindAll = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		exchangeMiddleware,
		objectPermissionChecker,
	}

	ms.BeforeCreate = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		dataValidationMiddleware,
		createEventHandler,
		exchangeMiddleware,
	}
	ms.AfterCreate = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		createEventHandler,
		exchangeMiddleware,
	}

	ms.BeforeDelete = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		deleteEventHandler,
		exchangeMiddleware,
	}
	ms.AfterDelete = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		deleteEventHandler,
		exchangeMiddleware,
	}

	ms.BeforeUpdate = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		dataValidationMiddleware,
		yhsHandler,
		updateEventHandler,
		exchangeMiddleware,
	}
	ms.AfterUpdate = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		updateEventHandler,
		exchangeMiddleware,
	}

	ms.BeforeFindOne = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		exchangeMiddleware,
	}
	ms.AfterFindOne = []resource.DatabaseRequestInterceptor{
		tablePermissionChecker,
		objectPermissionChecker,
		exchangeMiddleware,
	}
	return ms
}

func CleanUpConfigFiles() {

	files, _ := filepath.Glob("*_uploaded_*")
	log.Printf("Clean up uploaded config files: %v", files)

	for _, fileName := range files {
		err := os.Remove(fileName)
		resource.CheckErr(err, "Failed to delete uploaded schema file: %s", fileName)
	}

	schemaFolderDefinedByEnv, _ := os.LookupEnv("DAPTIN_SCHEMA_FOLDER")
	files, _ = filepath.Glob(schemaFolderDefinedByEnv + string(os.PathSeparator) + "*_uploaded_*")

	for _, fileName := range files {
		err := os.Remove(fileName)
		log.Printf("Deleted config files: %v", fileName)
		resource.CheckErr(err, "Failed to delete uploaded schema file: %s", fileName)
	}

}
