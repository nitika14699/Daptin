package resource

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/artpar/api2go"
	uuid "github.com/artpar/go.uuid"
	"github.com/daptin/daptin/server/auth"
	"github.com/dop251/goja"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	//"io"
	"crypto/md5"
	"encoding/hex"
	"io/ioutil"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"io"
	"net/url"
	"strconv"

	"github.com/artpar/conform"
	"gopkg.in/go-playground/validator.v9"
)

var guestActions = map[string]Action{}

func CreateGuestActionListHandler(initConfig *CmsConfig) func(*gin.Context) {

	actionMap := make(map[string]Action)

	for _, ac := range initConfig.Actions {
		actionMap[ac.OnType+":"+ac.Name] = ac
	}

	guestActions["user:signup"] = actionMap["user_account:signup"]
	guestActions["user:signin"] = actionMap["user_account:signin"]

	return func(c *gin.Context) {

		c.JSON(200, guestActions)
	}
}

type ActionPerformerInterface interface {
	DoAction(request Outcome, inFields map[string]interface{}) (api2go.Responder, []ActionResponse, []error)
	Name() string
}

type DaptinError struct {
	Message string
	Code    string
}

func (de *DaptinError) Error() string {
	return de.Message
}

func NewDaptinError(str string, code string) *DaptinError {
	return &DaptinError{
		Message: str,
		Code:    code,
	}
}

func CreatePostActionHandler(initConfig *CmsConfig,
	cruds map[string]*DbResource, actionPerformers []ActionPerformerInterface) func(*gin.Context) {

	actionMap := make(map[string]Action)

	for _, ac := range initConfig.Actions {
		actionMap[ac.OnType+":"+ac.Name] = ac
	}

	actionHandlerMap := make(map[string]ActionPerformerInterface)

	for _, actionPerformer := range actionPerformers {
		if actionPerformer == nil {
			continue
		}
		actionHandlerMap[actionPerformer.Name()] = actionPerformer
	}

	return func(ginContext *gin.Context) {

		actionName := ginContext.Param("actionName")
		actionType := ginContext.Param("typename")

		actionRequest, err := BuildActionRequest(ginContext.Request.Body, actionType, actionName,
			ginContext.Params, ginContext.Request.URL.Query())

		if err != nil {
			ginContext.Error(err)
			return
		}
		//log.Printf("Action Request body: %v", actionRequest)

		req := api2go.Request{
			PlainRequest: &http.Request{
				Method: "POST",
			},
		}

		req.PlainRequest = req.PlainRequest.WithContext(ginContext.Request.Context())

		actionCrudResource, ok := cruds[actionType]
		if !ok {
			actionCrudResource = cruds["world"]
		}

		responses, err := actionCrudResource.HandleActionRequest(actionRequest, req)

		responseStatus := 200
		for _, response := range responses {
			if response.ResponseType == "client.header.set" {
				attrs := response.Attributes.(map[string]string)

				for key, value := range attrs {
					if strings.ToLower(key) == "status" {
						responseStatusCode, err := strconv.ParseInt(value, 10, 32)
						if err != nil {
							log.Errorf("invalid status code value set in response: %v", value)
						} else {
							responseStatus = int(responseStatusCode)
						}
					} else {
						ginContext.Header(key, value)
					}
				}
			}
		}

		if err != nil {
			if httpErr, ok := err.(api2go.HTTPError); ok {
				if len(responses) > 0 {
					ginContext.AbortWithStatusJSON(httpErr.Status(), responses)
				} else {
					ginContext.AbortWithStatusJSON(httpErr.Status(), []ActionResponse{
						{
							ResponseType: "client.notify",
							Attributes: map[string]interface{}{
								"message": err.Error(),
								"title":   "failed",
								"type":    "error",
							},
						},
					})

				}
			} else {
				if len(responses) > 0 {
					ginContext.AbortWithStatusJSON(400, responses)
				} else {
					ginContext.AbortWithStatusJSON(500, []ActionResponse{
						{
							ResponseType: "client.notify",
							Attributes: map[string]interface{}{
								"message": err.Error(),
								"title":   "failed",
								"type":    "error",
							},
						},
					})
				}

			}
			return
		}

		//log.Printf("Final responses: %v", responses)

		ginContext.JSON(responseStatus, responses)

	}
}

func (db *DbResource) HandleActionRequest(actionRequest ActionRequest, req api2go.Request) ([]ActionResponse, error) {

	user := req.PlainRequest.Context().Value("user")
	sessionUser := &auth.SessionUser{}

	if user != nil {
		sessionUser = user.(*auth.SessionUser)
	}

	var subjectInstance *api2go.Api2GoModel
	var subjectInstanceMap map[string]interface{}

	action, err := db.GetActionByName(actionRequest.Type, actionRequest.Action)
	CheckErr(err, "Failed to get action by Type/action [%v][%v]", actionRequest.Type, actionRequest.Action)
	if err != nil {
		log.Warnf("invalid action: %v - %v", actionRequest.Action, actionRequest.Type)
		return nil, api2go.NewHTTPError(err, "no such action", 400)
	}

	isAdmin := db.IsAdmin(sessionUser.UserReferenceId)

	subjectInstanceReferenceId, ok := actionRequest.Attributes[actionRequest.Type+"_id"]
	if ok {
		req.PlainRequest.Method = "GET"
		req.QueryParams = make(map[string][]string)
		req.QueryParams["included_relations"] = action.RequestSubjectRelations
		referencedObject, err := db.FindOne(subjectInstanceReferenceId.(string), req)
		if err != nil {
			log.Warnf("failed to load subject for action: %v - %v", actionRequest.Action, subjectInstanceReferenceId)
			return nil, api2go.NewHTTPError(err, "failed to load subject", 400)
		}
		subjectInstance = referencedObject.Result().(*api2go.Api2GoModel)

		subjectInstanceMap = subjectInstance.Data

		if subjectInstanceMap == nil {
			log.Warnf("subject is empty: %v - %v", actionRequest.Action, subjectInstanceReferenceId)
			return nil, api2go.NewHTTPError(errors.New("subject not found"), "subject not found", 400)
		}

		subjectInstanceMap["__type"] = subjectInstance.GetName()
		permission := db.GetRowPermission(subjectInstanceMap)

		if !permission.CanExecute(sessionUser.UserReferenceId, sessionUser.Groups) {
			log.Warnf("user not allowed action on this object: %v - %v", actionRequest.Action, subjectInstanceReferenceId)
			return nil, api2go.NewHTTPError(errors.New("forbidden"), "forbidden", 403)
		}
	}

	if !isAdmin && !db.IsUserActionAllowed(sessionUser.UserReferenceId, sessionUser.Groups, actionRequest.Type, actionRequest.Action) {
		log.Warnf("user not allowed action: %v - %v", actionRequest.Action, subjectInstanceReferenceId)
		return nil, api2go.NewHTTPError(errors.New("forbidden"), "forbidden", 403)
	}

	//log.Printf("Handle event for action [%v]", actionRequest.Action)

	if !action.InstanceOptional && (subjectInstanceReferenceId == "" || subjectInstance == nil) {
		log.Warnf("subject is unidentified: %v - %v", actionRequest.Action, actionRequest.Type)
		return nil, api2go.NewHTTPError(errors.New("required reference id not provided or incorrect"), "no reference id", 400)
	}

	if actionRequest.Attributes == nil {
		actionRequest.Attributes = make(map[string]interface{})
	}

	for _, field := range action.InFields {
		_, ok := actionRequest.Attributes[field.ColumnName]
		if !ok {
			actionRequest.Attributes[field.ColumnName] = req.PlainRequest.Form.Get(field.ColumnName)
		}
	}

	for _, validation := range action.Validations {
		errs := ValidatorInstance.VarWithValue(actionRequest.Attributes[validation.ColumnName], actionRequest.Attributes, validation.Tags)
		if errs != nil {
			log.Warnf("validation on input fields failed: %v - %v", actionRequest.Action, actionRequest.Type)
			validationErrors := errs.(validator.ValidationErrors)
			firstError := validationErrors[0]
			return nil, api2go.NewHTTPError(errors.New(fmt.Sprintf("invalid value for %s", validation.ColumnName)), firstError.Tag(), 400)
		}
	}

	for _, conformations := range action.Conformations {

		val, ok := actionRequest.Attributes[conformations.ColumnName]
		if !ok {
			continue
		}
		valStr, ok := val.(string)
		if !ok {
			continue
		}
		newVal := conform.TransformString(valStr, conformations.Tags)
		actionRequest.Attributes[conformations.ColumnName] = newVal
	}

	inFieldMap, err := GetValidatedInFields(actionRequest, action)
	inFieldMap["attributes"] = actionRequest.Attributes

	if err != nil {
		return nil, api2go.NewHTTPError(err, "failed to validate fields", 400)
	}

	if sessionUser.UserReferenceId != "" {
		user, err := db.GetReferenceIdToObject(USER_ACCOUNT_TABLE_NAME, sessionUser.UserReferenceId)
		if err != nil {
			return nil, api2go.NewHTTPError(err, "failed to identify user", 401)
		}
		inFieldMap["user"] = user
	}

	if subjectInstanceMap != nil {
		inFieldMap[actionRequest.Type+"_id"] = subjectInstanceMap["reference_id"]
		inFieldMap["subject"] = subjectInstanceMap
	}

	responses := make([]ActionResponse, 0)

OutFields:
	for _, outcome := range action.OutFields {
		var responseObjects interface{}
		responseObjects = nil
		var responses1 []ActionResponse
		var errors1 []error
		var actionResponse ActionResponse

		log.Printf("Action [%v][%v] => Outcome [%v][%v] ", actionRequest.Action, subjectInstanceReferenceId, outcome.Type, outcome.Method)

		if len(outcome.Condition) > 0 {
			outcomeResult, err := evaluateString(outcome.Condition, inFieldMap)
			CheckErr(err, "Failed to evaluate condition, assuming false by default")
			if err != nil {
				continue
			}

			log.Printf("Evaluated condition [%v] result: %v", outcome.Condition, outcomeResult)
			boolValue, ok := outcomeResult.(bool)
			if !ok {

				strVal, ok := outcomeResult.(string)
				if ok {
					if strVal == "1" || strings.ToLower(strings.TrimSpace(strVal)) == "true" {
						log.Printf("Condition is true")
						// condition is true
					} else {
						// condition isn't true
						log.Printf("Condition is false, skipping outcome")
						continue
					}

				} else {

					log.Printf("Failed to convert value to bool, assuming false")
					continue
				}

			} else if !boolValue {
				log.Printf("Outcome [%v][%v] skipped because condition failed [%v]", outcome.Method, outcome.Type, outcome.Condition)
				continue
			}
		}

		model, request, err := BuildOutcome(inFieldMap, outcome)
		if err != nil {
			log.Errorf("Failed to build outcome: %v", err)
			log.Errorf("Infields - %v", toJson(inFieldMap))
			responses = append(responses, NewActionResponse("error", "Failed to build outcome "+outcome.Type))
			if outcome.ContinueOnError {
				continue
			} else {
				return []ActionResponse{}, fmt.Errorf("invalid input for %v", outcome.Type)
			}
		}

		requestContext := req.PlainRequest.Context()
		var adminUserReferenceId string
		adminUserReferenceIds := db.GetAdminReferenceId()
		for id, _ := range adminUserReferenceIds {
			adminUserReferenceId = id
			break
		}

		if len(adminUserReferenceId) > 0 {
			requestContext = context.WithValue(requestContext, "user", &auth.SessionUser{
				UserReferenceId: adminUserReferenceId,
			})
		}
		request.PlainRequest = request.PlainRequest.WithContext(requestContext)
		dbResource, _ := db.Cruds[outcome.Type]

		actionResponses := make([]ActionResponse, 0)
		//log.Printf("Next outcome method: [%v][%v]", outcome.Method, outcome.Type)
		switch outcome.Method {
		case "POST":
			responseObjects, err = dbResource.Create(model, request)
			CheckErr(err, "Failed to post from action")
			if err != nil {

				actionResponse = NewActionResponse("client.notify", NewClientNotification("error", "Failed to create "+model.GetName()+". "+err.Error(), "Failed"))
				responses = append(responses, actionResponse)
				break OutFields
			} else {
				createdRow := responseObjects.(api2go.Response).Result().(*api2go.Api2GoModel).Data
				actionResponse = NewActionResponse(createdRow["__type"].(string), createdRow)
			}
			actionResponses = append(actionResponses, actionResponse)
		case "GET":

			request.QueryParams = make(map[string][]string)

			for k, val := range model.Data {
				if k == "query" {
					request.QueryParams[k] = []string{toJson(val)}
				} else {
					request.QueryParams[k] = []string{fmt.Sprintf("%v", val)}
				}
			}

			responseObjects, _, _, _, err = dbResource.PaginatedFindAllWithoutFilters(request)
			CheckErr(err, "Failed to get inside action")
			if err != nil {
				actionResponse = NewActionResponse("client.notify",
					NewClientNotification("error", "Failed to get "+model.GetName()+". "+err.Error(), "Failed"))
				responses = append(responses, actionResponse)
				break OutFields
			} else {
				actionResponse = NewActionResponse(actionRequest.Type, responseObjects)
			}
			actionResponses = append(actionResponses, actionResponse)
		case "GET_BY_ID":

			referenceId, ok := model.Data["reference_id"]
			if referenceId == nil || !ok {
				return nil, api2go.NewHTTPError(err, "no reference id provided for GET_BY_ONE", 400)
			}

			includedRelations := make(map[string]bool, 0)
			if model.Data["included_relations"] != nil {
				//included := req.QueryParams["included_relations"][0]
				//includedRelationsList := strings.Split(included, ",")
				for _, incl := range strings.Split(model.Data["included_relations"].(string), ",") {
					includedRelations[incl] = true
				}

			} else {
				includedRelations = nil
			}

			responseObjects, _, err = dbResource.GetSingleRowByReferenceId(outcome.Type, referenceId.(string), nil)
			CheckErr(err, "Failed to get by id")

			if err != nil {
				actionResponse = NewActionResponse("client.notify",
					NewClientNotification("error", "Failed to create "+model.GetName()+". "+err.Error(), "Failed"))
				responses = append(responses, actionResponse)
				break OutFields
			} else {
				actionResponse = NewActionResponse(actionRequest.Type, responseObjects)
			}
			actionResponses = append(actionResponses, actionResponse)
		case "PATCH":
			responseObjects, err = dbResource.Update(model, request)
			CheckErr(err, "Failed to update inside action")
			if err != nil {
				actionResponse = NewActionResponse("client.notify", NewClientNotification("error", "Failed to update "+model.GetName()+". "+err.Error(), "Failed"))
				responses = append(responses, actionResponse)
				break OutFields
			} else {
				createdRow := responseObjects.(api2go.Response).Result().(*api2go.Api2GoModel).Data
				actionResponse = NewActionResponse(createdRow["__type"].(string), createdRow)
			}
			actionResponses = append(actionResponses, actionResponse)
		case "DELETE":
			err = dbResource.DeleteWithoutFilters(model.Data["reference_id"].(string), request)
			CheckErr(err, "Failed to delete inside action")
			if err != nil {
				actionResponse = NewActionResponse("client.notify", NewClientNotification("error", "Failed to delete "+model.GetName(), "Failed"))
				responses = append(responses, actionResponse)
				break OutFields
			} else {
				actionResponse = NewActionResponse("client.notify", NewClientNotification("success", "Deleted "+model.GetName(), "Success"))
			}
			actionResponses = append(actionResponses, actionResponse)
		case "EXECUTE":
			//res, err = Cruds[outcome.Type].Create(model, actionRequest)

			actionName := model.GetName()
			performer, ok := db.ActionHandlerMap[actionName]
			if !ok {
				log.Errorf("Invalid outcome method: [%v]%v", outcome.Method, model.GetName())
				//return ginContext.AbortWithError(500, errors.New("Invalid outcome"))
			} else {
				var responder api2go.Responder
				outcome.Attributes["user"] = sessionUser
				responder, responses1, errors1 = performer.DoAction(outcome, model.Data)
				actionResponses = append(actionResponses, responses1...)
				if len(errors1) > 0 {
					err = errors1[0]
				}
				if responder != nil {
					responseObjects = responder.Result().(*api2go.Api2GoModel).Data
				}
			}

		case "ACTIONRESPONSE":
			//res, err = Cruds[outcome.Type].Create(model, actionRequest)
			log.Printf("Create action response: %v", model.GetName())
			var actionResponse ActionResponse
			actionResponse = NewActionResponse(model.GetName(), model.Data)
			actionResponses = append(actionResponses, actionResponse)
		default:
			handler, ok := db.ActionHandlerMap[outcome.Type]

			if !ok {
				log.Errorf("Unknown method invoked onn %v: %v", outcome.Type, outcome.Method)
				continue
			}
			responder, responses1, err1 := handler.DoAction(outcome, model.Data)
			if err1 != nil {
				err = err1[0]
			} else {
				actionResponses = append(actionResponses, responses1...)
				responseObjects = responder
			}

		}

		if !outcome.SkipInResponse {
			responses = append(responses, actionResponses...)
		}

		if len(actionResponses) > 0 && outcome.Reference != "" {
			lst := make([]interface{}, 0)
			for i, res := range actionResponses {
				inFieldMap[fmt.Sprintf("response.%v[%v]", outcome.Reference, i)] = res.Attributes
				lst = append(lst, res.Attributes)
			}
			inFieldMap[fmt.Sprintf("%v", outcome.Reference)] = lst
		}

		if responseObjects != nil && outcome.Reference != "" {

			api2goModel, ok := responseObjects.(api2go.Response)
			if ok {
				responseObjects = api2goModel.Result().(*api2go.Api2GoModel).Data
			}

			singleResult, isSingleResult := responseObjects.(map[string]interface{})

			if isSingleResult {
				inFieldMap[outcome.Reference] = singleResult
			} else {
				resultArray, ok := responseObjects.([]map[string]interface{})

				finalArray := make([]map[string]interface{}, 0)
				if ok {
					for i, item := range resultArray {
						finalArray = append(finalArray, item)
						inFieldMap[fmt.Sprintf("%v[%v]", outcome.Reference, i)] = item
					}
				}
				inFieldMap[outcome.Reference] = finalArray

			}
		}

		if err != nil {
			return responses, err
		}
	}

	return responses, nil
}

func BuildActionRequest(closer io.ReadCloser, actionType, actionName string,
	params gin.Params, queryParams url.Values) (ActionRequest, error) {
	bytes, err := ioutil.ReadAll(closer)
	actionRequest := ActionRequest{}
	if err != nil {
		return actionRequest, err
	}

	err = json.Unmarshal(bytes, &actionRequest)
	CheckErr(err, "Failed to read request body as json")
	if err != nil {
		values, err := url.ParseQuery(string(bytes))
		CheckErr(err, "Failed to parse body as query values")
		if err == nil {

			attributesMap := make(map[string]interface{})
			actionRequest.Attributes = make(map[string]interface{})
			for key, val := range values {
				if len(val) > 1 {
					attributesMap[key] = val
					actionRequest.Attributes[key] = val
				} else {
					attributesMap[key] = val[0]
					actionRequest.Attributes[key] = val[0]
				}
			}
			attributesMap["__body"] = string(bytes)
			actionRequest.Attributes = attributesMap
		}
	}

	if actionRequest.Attributes == nil {
		actionRequest.Attributes = make(map[string]interface{})
	}

	var data map[string]interface{}
	err = json.Unmarshal(bytes, &data)
	CheckErr(err, "Failed to read body as json", data)
	for k, v := range data {
		if k == "attributes" {
			continue
		}
		actionRequest.Attributes[k] = v
	}

	actionRequest.Type = actionType
	actionRequest.Action = actionName

	if actionRequest.Attributes == nil {
		actionRequest.Attributes = make(map[string]interface{})
	}
	for _, param := range params {
		actionRequest.Attributes[param.Key] = param.Value
	}
	for key, valueArray := range queryParams {

		if len(valueArray) == 1 {
			actionRequest.Attributes[key] = valueArray[0]
		} else {
			actionRequest.Attributes[key] = valueArray
		}
	}

	return actionRequest, nil
}

func NewClientNotification(notificationType string, message string, title string) map[string]interface{} {

	m := make(map[string]interface{})

	m["type"] = notificationType
	m["message"] = message
	m["title"] = title
	return m

}

func GetMD5HashString(text string) string {
	return GetMD5Hash([]byte(text))
}

func GetMD5Hash(text []byte) string {
	hasher := md5.New()
	hasher.Write(text)
	return hex.EncodeToString(hasher.Sum(nil))
}

type ActionResponse struct {
	ResponseType string
	Attributes   interface{}
}

func NewActionResponse(responseType string, attrs interface{}) ActionResponse {

	ar := ActionResponse{
		ResponseType: responseType,
		Attributes:   attrs,
	}

	return ar

}

func BuildOutcome(inFieldMap map[string]interface{}, outcome Outcome) (*api2go.Api2GoModel, api2go.Request, error) {

	attrInterface, err := BuildActionContext(outcome.Attributes, inFieldMap)
	if err != nil {
		return nil, api2go.Request{}, err
	}
	attrs := attrInterface.(map[string]interface{})

	switch outcome.Type {
	case "system_json_schema_update":
		responseModel := api2go.NewApi2GoModel("__restart", nil, 0, nil)
		returnRequest := api2go.Request{
			PlainRequest: &http.Request{
				Method: "EXECUTE",
			},
		}

		files1, ok := attrs["json_schema"]
		if !ok {
			return nil, returnRequest, errors.New("no files uploaded")
		}
		log.Printf("Files [%v]: %v", attrs, files1)
		files, ok := files1.([]interface{})
		if !ok || len(files) < 1 {
			return nil, returnRequest, errors.New("no files uploaded")
		}
		for _, file := range files {
			f := file.(map[string]interface{})
			fileName := f["name"].(string)
			log.Printf("File name: %v", fileName)
			fileNameParts := strings.Split(fileName, ".")
			fileFormat := fileNameParts[len(fileNameParts)-1]
			contents := f["file"]
			if contents == nil {
				contents = f["contents"]
			}
			if contents == nil {
				log.Printf("Contents are missing in the update schema request: %v", f)
				continue
			}
			fileContentsBase64 := contents.(string)
			var fileBytes []byte
			contentParts := strings.Split(fileContentsBase64, ",")
			if len(contentParts) > 1 {
				fileBytes, err = base64.StdEncoding.DecodeString(contentParts[1])
			} else {
				fileBytes, err = base64.StdEncoding.DecodeString(fileContentsBase64)
			}
			if err != nil {
				return nil, returnRequest, err
			}

			jsonFileName := fmt.Sprintf("schema_uploaded_%v_daptin.%v", fileName, fileFormat)
			err = ioutil.WriteFile(jsonFileName, fileBytes, 0644)
			if err != nil {
				log.Errorf("Failed to write json file: %v", jsonFileName)
				return nil, returnRequest, err
			}

		}

		log.Printf("Written all json files. Attempting restart")

		return responseModel, returnRequest, nil

	case "__download_cms_config":
		fallthrough
	case "__become_admin":

		returnRequest := api2go.Request{
			PlainRequest: &http.Request{
				Method: "EXECUTE",
			},
		}
		model := api2go.NewApi2GoModelWithData(outcome.Type, nil, int64(auth.DEFAULT_PERMISSION), nil, attrs)

		return model, returnRequest, nil

	case "action.response":
		fallthrough
	case "client.redirect":
		fallthrough
	case "client.store.set":
		fallthrough
	case "client.notify":
		//respopnseModel := NewActionResponse(attrs["responseType"].(string), attrs)
		returnRequest := api2go.Request{
			PlainRequest: &http.Request{
				Method: "ACTIONRESPONSE",
			},
		}
		model := api2go.NewApi2GoModelWithData(outcome.Type, nil, int64(auth.DEFAULT_PERMISSION), nil, attrs)

		return model, returnRequest, err

	default:

		model := api2go.NewApi2GoModelWithData(outcome.Type, nil, int64(auth.DEFAULT_PERMISSION), nil, attrs)

		req := api2go.Request{
			PlainRequest: &http.Request{
				Method: outcome.Method,
			},
		}
		return model, req, err

	}

	//return nil, api2go.Request{}, errors.New(fmt.Sprintf("Unidentified outcome: %v", outcome.Type))

}

func runUnsafeJavascript(unsafe string, contextMap map[string]interface{}) (interface{}, error) {

	vm := goja.New()

	//vm.ToValue(contextMap)
	for key, val := range contextMap {
		vm.Set(key, val)
	}

	vm.Set("btoa", func(data []byte) string {
		return base64.StdEncoding.EncodeToString(data)
	})

	vm.Set("uuid", func() string {
		u, _ := uuid.NewV4()
		return u.String()
	})
	v, err := vm.RunString(unsafe) // Here be dragons (risky code)

	if err != nil {
		return nil, err
	}

	return v.Export(), nil
}

func BuildActionContext(outcomeAttributes interface{}, inFieldMap map[string]interface{}) (interface{}, error) {

	var data interface{}

	kindOfOutcome := reflect.TypeOf(outcomeAttributes).Kind()

	if kindOfOutcome == reflect.Map {

		dataMap := make(map[string]interface{})

		outcomeMap := outcomeAttributes.(map[string]interface{})
		for key, field := range outcomeMap {

			typeOfField := reflect.TypeOf(field).Kind()
			//log.Printf("Outcome attribute [%v] == %v [%v]", key, field, typeOfField)

			if typeOfField == reflect.String {

				fieldString := field.(string)

				val, err := evaluateString(fieldString, inFieldMap)
				//log.Printf("Value of [%v] == [%v]", key, val)
				if err != nil {
					return data, err
				}
				if val != nil {
					dataMap[key] = val
				}

			} else if typeOfField == reflect.Map || typeOfField == reflect.Slice || typeOfField == reflect.Array {

				val, err := BuildActionContext(field, inFieldMap)
				if err != nil {
					return data, err
				}
				if val != nil {
					dataMap[key] = val
				}
			} else {
				dataMap[key] = field
			}

		}

		data = dataMap

	} else if kindOfOutcome == reflect.Array || kindOfOutcome == reflect.Slice {

		outcomeArray, ok := outcomeAttributes.([]interface{})

		if !ok {
			outcomeArray = make([]interface{}, 0)
			outcomeArrayString := outcomeAttributes.([]string)
			for _, o := range outcomeArrayString {
				outcomeArray = append(outcomeArray, o)
			}
		}

		outcomes := make([]interface{}, 0)

		for _, outcome := range outcomeArray {

			outcomeKind := reflect.TypeOf(outcome).Kind()

			if outcomeKind == reflect.String {

				outcomeString := outcome.(string)

				evtStr, err := evaluateString(outcomeString, inFieldMap)
				if err != nil {
					return data, err
				}
				outcomes = append(outcomes, evtStr)

			} else if outcomeKind == reflect.Map || outcomeKind == reflect.Array || outcomeKind == reflect.Slice {
				outc, err := BuildActionContext(outcome, inFieldMap)
				//log.Printf("Outcome is: %v", outc)
				if err != nil {
					return data, err
				}
				outcomes = append(outcomes, outc)
			}

		}
		data = outcomes

	}

	return data, nil
}

func evaluateString(fieldString string, inFieldMap map[string]interface{}) (interface{}, error) {

	var val interface{}

	if fieldString == "" {
		return "", nil
	}

	if fieldString[0] == '!' {

		res, err := runUnsafeJavascript(fieldString[1:], inFieldMap)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate JS in outcome attribute for key %s: %v", fieldString, err)
		}
		val = res

	} else if len(fieldString) > 3 && BeginsWith(fieldString, "{{") && EndsWithCheck(fieldString, "}}") {

		jsString := fieldString[2 : len(fieldString)-2]
		res, err := runUnsafeJavascript(jsString, inFieldMap)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate JS in outcome attribute for key %s: %v", fieldString, err)
		}
		val = res

	} else if len(fieldString) > 3 && fieldString[0:3] == "js:" {

		res, err := runUnsafeJavascript(fieldString[1:], inFieldMap)
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate JS in outcome attribute for key %s: %v", fieldString, err)
		}
		val = res

	} else if fieldString[0] == '~' {

		fieldParts := strings.Split(fieldString[1:], ".")

		if fieldParts[0] == "" {
			fieldParts[0] = "subject"
		}
		var finalValue interface{}

		// it looks confusing but it does whats its supposed to do
		// todo: add helpful comment

		finalValue = inFieldMap
		for i := 0; i < len(fieldParts)-1; i++ {
			fieldPart := fieldParts[i]
			finalValue = finalValue.(map[string]interface{})[fieldPart]
		}
		if finalValue == nil {
			return nil, nil
		}

		castMap := finalValue.(map[string]interface{})
		finalValue = castMap[fieldParts[len(fieldParts)-1]]
		val = finalValue

	} else {
		//log.Printf("Get [%v] from infields: %v", fieldString, toJson(inFieldMap))

		rex := regexp.MustCompile(`\$([a-zA-Z0-9_\[\]]+)?(\.[a-zA-Z0-9_\[\]]+)*`)
		matches := rex.FindAllStringSubmatch(fieldString, -1)

		for _, match := range matches {

			fieldParts := strings.Split(match[0][1:], ".")

			if fieldParts[0] == "" {
				fieldParts[0] = "subject"
			}

			var finalValue interface{}

			// it looks confusing but it does whats its supposed to do
			// todo: add helpful comment

			finalValue = inFieldMap
			for i := 0; i < len(fieldParts)-1; i++ {

				fieldPart := fieldParts[i]

				fieldIndexParts := strings.Split(fieldPart, "[")
				if len(fieldIndexParts) > 1 {
					right := strings.Split(fieldIndexParts[1], "]")
					index, err := strconv.ParseInt(right[0], 10, 64)
					if err == nil {
						finalValMap := finalValue.(map[string]interface{})
						mapPart := finalValMap[fieldIndexParts[0]]
						mapPartArray, ok := mapPart.([]map[string]interface{})
						if !ok {
							mapPartArrayInterface, ok := mapPart.([]interface{})
							if ok {
								mapPartArray = make([]map[string]interface{}, 0)
								for _, ar := range mapPartArrayInterface {
									mapPartArray = append(mapPartArray, ar.(map[string]interface{}))
								}
							}
						}

						if int(index) > len(mapPartArray)-1 {
							return nil, fmt.Errorf("failed to evaluate value from array in outcome attribute for key %s, index [%d] is out of range [%d values]: %v", fieldString, index, len(mapPartArray), err)
						}
						finalValue = mapPartArray[index]
					} else {
						finalValue = finalValue.(map[string]interface{})[fieldPart]
					}
				} else {
					var ok bool
					finalValue, ok = finalValue.(map[string]interface{})[fieldPart]
					if !ok {
						return nil, fmt.Errorf("failed to evaluate value from array in outcome attribute for key %s, value is nil", fieldString)
					}

				}

			}
			if finalValue == nil {
				return nil, nil
			}

			castMap, ok := finalValue.(map[string]interface{})
			if !ok {
				log.Errorf("Value at [%v] is %v", fieldString, castMap)
				return val, errors.New(fmt.Sprintf("unable to evaluate value for [%v]", fieldString))
			}
			finalValue = castMap[fieldParts[len(fieldParts)-1]]
			fieldString = strings.Replace(fieldString, fmt.Sprintf("%v", match[0]), fmt.Sprintf("%v", finalValue), -1)
		}
		val = fieldString

	}
	//log.Printf("Evaluated string path [%v] => %v", fieldString, val)

	return val, nil
}

func GetValidatedInFields(actionRequest ActionRequest, action Action) (map[string]interface{}, error) {

	dataMap := actionRequest.Attributes
	finalDataMap := make(map[string]interface{})

	for _, inField := range action.InFields {
		val, ok := dataMap[inField.ColumnName]
		if ok {
			finalDataMap[inField.ColumnName] = val

		} else if inField.DefaultValue != "" {
		} else if inField.IsNullable {

		} else {
			return nil, errors.New(fmt.Sprintf("Field %s cannot be blank", inField.Name))
		}
	}

	return finalDataMap, nil
}
