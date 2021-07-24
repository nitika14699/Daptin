package resource

import (
	"errors"
	"github.com/artpar/go-imap"
	"github.com/artpar/go-imap/backend"
	"github.com/daptin/daptin/server/auth"
)

type DaptinImapBackend struct {
	cruds map[string]*DbResource
}

func (be *DaptinImapBackend) LoginMd5(conn *imap.ConnInfo, username, challenge string, response string) (backend.User, error) {

	//userMailAccount, err := be.cruds[USER_ACCOUNT_TABLE_NAME].GetUserMailAccountRowByEmail(username)
	//if err != nil {
	//	return nil, err
	//}

	//userAccount, _, err := be.cruds[USER_ACCOUNT_TABLE_NAME].GetSingleRowByReferenceId("user_account", userMailAccount["user_account_id"].(string))
	//userId, _ := userAccount["id"].(int64)
	//groups := be.cruds[USER_ACCOUNT_TABLE_NAME].GetObjectUserGroupsByWhere("user_account", "id", userId)

	//sessionUser := &auth.SessionUser{
	//	UserId:          userId,
	//	UserReferenceId: userAccount["reference_id"].(string),
	//	Groups:          groups,
	//}

	//if HmacCheckStringHash(response, challenge, userMailAccount["password_md5"].(string)) {
	//
	//	return &DaptinImapUser{
	//		username:               username,
	//		mailAccountId:          userMailAccount["id"].(int64),
	//		mailAccountReferenceId: userMailAccount["reference_id"].(string),
	//		dbResource:             be.cruds,
	//		sessionUser:            sessionUser,
	//	}, nil
	//}

	return nil, errors.New("md5 based login not supported")

}

func (be *DaptinImapBackend) Login(conn *imap.ConnInfo, username, password string) (backend.User, error) {

	userMailAccount, err := be.cruds[USER_ACCOUNT_TABLE_NAME].GetUserMailAccountRowByEmail(username)
	if err != nil {
		return nil, err
	}

	userAccount, _, err := be.cruds[USER_ACCOUNT_TABLE_NAME].GetSingleRowByReferenceId("user_account", userMailAccount["user_account_id"].(string), nil)
	userId, _ := userAccount["id"].(int64)
	groups := be.cruds[USER_ACCOUNT_TABLE_NAME].GetObjectUserGroupsByWhere("user_account", "id", userId)

	sessionUser := &auth.SessionUser{
		UserId:          userId,
		UserReferenceId: userAccount["reference_id"].(string),
		Groups:          groups,
	}

	if BcryptCheckStringHash(password, userMailAccount["password"].(string)) {

		return &DaptinImapUser{
			username:               username,
			mailAccountId:          userMailAccount["id"].(int64),
			mailAccountReferenceId: userMailAccount["reference_id"].(string),
			dbResource:             be.cruds,
			sessionUser:            sessionUser,
		}, nil
	}

	return nil, errors.New("bad username or password")
}

func NewImapServer(cruds map[string]*DbResource) *DaptinImapBackend {
	return &DaptinImapBackend{
		cruds: cruds,
	}
}
