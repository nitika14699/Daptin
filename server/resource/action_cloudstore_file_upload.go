package resource

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/artpar/go.uuid"
	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io/ioutil"
	sync2 "sync"
	"time"

	//"os"
	"archive/zip"
	"github.com/artpar/api2go"
	"github.com/artpar/rclone/fs/config"
	"github.com/artpar/rclone/fs/sync"
	"golang.org/x/oauth2"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type fileUploadActionPerformer struct {
	cruds map[string]*DbResource
}

func (d *fileUploadActionPerformer) Name() string {
	return "cloudstore.file.upload"
}

func unzip(archive, target string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	for _, file := range reader.File {
		path := filepath.Join(target, file.Name)
		if file.FileInfo().IsDir() {
			os.MkdirAll(path, file.Mode())
			continue
		}

		fileReader, err := file.Open()
		if err != nil {
			return err
		}
		defer fileReader.Close()

		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
		if err != nil {
			return err
		}
		defer targetFile.Close()

		if _, err := io.Copy(targetFile, fileReader); err != nil {
			return err
		}
	}

	return nil
}

func EndsWithCheck(str string, endsWith string) bool {
	if len(endsWith) > len(str) {
		return false
	}

	if len(endsWith) == len(str) && endsWith != str {
		return false
	}

	suffix := str[len(str)-len(endsWith):]
	i := suffix == endsWith
	return i

}
func EndsWith(str string, endsWith string) (string, bool) {
	if len(endsWith) > len(str) {
		return "", false
	}

	if len(endsWith) == len(str) && endsWith != str {
		return "", false
	}

	suffix := str[len(str)-len(endsWith):]
	prefix := str[:len(str)-len(endsWith)]
	i := suffix == endsWith
	return prefix, i

}

var cleanupmux = sync2.Mutex{}
var cleanuppath = make(map[string]bool)

func (d *fileUploadActionPerformer) DoAction(request Outcome, inFields map[string]interface{}) (api2go.Responder, []ActionResponse, []error) {

	responses := make([]ActionResponse, 0)

	u, _ := uuid.NewV4()
	sourceDirectoryName := "upload-" + u.String()[0:8]
	tempDirectoryPath, err := ioutil.TempDir(os.Getenv("DAPTIN_CACHE_FOLDER"), sourceDirectoryName)
	log.Printf("Temp directory for this upload fileUploadActionPerformer: %v", tempDirectoryPath)

	//defer os.RemoveAll(tempDirectoryPath) // clean up

	CheckErr(err, "Failed to create temp tempDirectoryPath for rclone upload")
	atPath, ok := inFields["path"].(string)
	files, ok := inFields["file"].([]interface{})
	if ok {

		for _, fileInterface := range files {
			file := fileInterface.(map[string]interface{})
			fileName, ok := file["name"].(string)
			if !ok {
				log.Errorf("Name is missing for file")
				continue
			}
			temproryFilePath := filepath.Join(tempDirectoryPath, fileName)

			fileContentsBase64, ok := file["file"].(string)
			if !ok {
				fileContentsBase64, ok = file["contents"].(string)
				if !ok {
					continue
				}
			}
			splitParts := strings.Split(fileContentsBase64, ",")
			encodedPart := splitParts[0]
			if len(splitParts) > 1 {
				encodedPart = splitParts[1]
			}
			fileBytes, err := base64.StdEncoding.DecodeString(encodedPart)
			log.Printf("Write file [%v] for upload", temproryFilePath)
			CheckErr(err, "Failed to convert base64 to []bytes")

			os.MkdirAll(tempDirectoryPath, 0600)

			err = ioutil.WriteFile(temproryFilePath, fileBytes, 0666)
			CheckErr(err, "Failed to write file bytes to temp file for rclone upload")

			if EndsWithCheck(fileName, ".zip") {
				err = unzip(temproryFilePath, tempDirectoryPath)
				CheckErr(err, "Failed to unzip file")
				go func() {
					time.Sleep(5 * time.Minute)
					err = os.Remove(temproryFilePath)
					CheckErr(err, "Failed to remove zip file after extraction")
				}()

			}

		}
		CheckErr(err, "Failed to remove cache folder: %s", tempDirectoryPath)
	} else {
		return nil, nil, []error{errors.New("improper file attachment")}
	}

	rootPath := inFields["root_path"].(string)
	if atPath != "" {

		if !EndsWithCheck(rootPath, "/") && len(atPath) > 0 && atPath[0] != '/' {
			rootPath = rootPath + "/"
		}
		rootPath = rootPath + atPath
	}
	args := []string{
		tempDirectoryPath,
		rootPath,
	}
	log.Printf("Upload source target %v %v", tempDirectoryPath, rootPath)

	var token *oauth2.Token
	oauthConf := &oauth2.Config{}
	oauthTokenId1 := inFields["oauth_token_id"]
	if oauthTokenId1 == nil {
		log.Printf("No oauth token set for target store")
	} else {
		oauthTokenId := oauthTokenId1.(string)
		token, oauthConf, err = d.cruds["oauth_token"].GetTokenByTokenReferenceId(oauthTokenId)
		CheckErr(err, "Failed to get oauth2 token for store sync")
	}

	jsonToken, err := json.Marshal(token)
	CheckErr(err, "Failed to marshal access token to json")

	storeProvider := inFields["store_provider"].(string)
	config.FileSet(storeProvider, "client_id", oauthConf.ClientID)
	config.FileSet(storeProvider, "type", storeProvider)
	config.FileSet(storeProvider, "client_secret", oauthConf.ClientSecret)
	config.FileSet(storeProvider, "token", string(jsonToken))
	config.FileSet(storeProvider, "client_scopes", strings.Join(oauthConf.Scopes, ","))
	config.FileSet(storeProvider, "redirect_url", oauthConf.RedirectURL)

	fsrc, fdst := cmd.NewFsSrcDst(args)
	cobraCommand := &cobra.Command{
		Use: fmt.Sprintf("File upload action from [%v]", tempDirectoryPath),
	}
	defaultConfig := fs.GetConfig(nil)

	defaultConfig.LogLevel = fs.LogLevelNotice

	go cmd.Run(true, false, cobraCommand, func() error {
		if fsrc == nil || fdst == nil {
			log.Errorf("Source or destination is null")
			return nil
		}

		ctx := context.Background()

		defaultConfig.DeleteMode = fs.DeleteModeOff
		err := sync.CopyDir(ctx, fdst, fsrc, true)
		InfoErr(err, "Failed to sync files for upload to cloud")

		go func() {
			cleanupmux.Lock()
			_, ok := cleanuppath[tempDirectoryPath]
			if ok {
				cleanupmux.Unlock()
				return
			}
			cleanuppath[tempDirectoryPath] = true
			cleanupmux.Unlock()

			time.Sleep(10 * time.Minute)
			cleanupmux.Lock()
			delete(cleanuppath, tempDirectoryPath)
			cleanupmux.Unlock()
			err = os.RemoveAll(tempDirectoryPath)
			InfoErr(err, "Failed to remove temp directory after upload")
		}()

		return err
	})

	restartAttrs := make(map[string]interface{})
	restartAttrs["type"] = "success"
	restartAttrs["message"] = "Cloud storage file upload queued"
	restartAttrs["title"] = "Success"
	actionResponse := NewActionResponse("client.notify", restartAttrs)
	responses = append(responses, actionResponse)

	return nil, responses, nil
}

func NewFileUploadActionPerformer(cruds map[string]*DbResource) (ActionPerformerInterface, error) {

	handler := fileUploadActionPerformer{
		cruds: cruds,
	}

	return &handler, nil

}
