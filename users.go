package gus

import (
	"database/sql"
	"github.com/asaskevich/govalidator"
	"github.com/satori/go.uuid"
	"golang.org/x/crypto/bcrypt"
	"strings"
	"time"
)

var (
	ErrEmailTaken              = ErrInvalid("That email is taken.")
	ErrUsernameTaken           = ErrInvalid("That username is taken.")
	ErrEmailInvalid            = ErrInvalid("'email' invalid.")
	ErrEmailRequired           = ErrInvalid("'email' required.")
	ErrUsernameRequired        = ErrInvalid("'username' required.")
	ErrUsernameOrEmailRequired = ErrInvalid("'username' or 'email' required.")
	ErrPasswordRequired        = ErrInvalid("'password' required.")
	ErrInvalidResetToken       = ErrInvalid("Invalid reset token.")
	ErrPasswordInvalid         = ErrInvalid(
		"'new_password' must contain: 1 Upper, 1 Lower, 1 Number, 1 Special and 8 Chars",
		"OR any alphanumeric with a minimum of 15 chars.")
)

type Role int64

type UserOpts struct {
	AuthAttempts     int64       // Maximum amount of times a user can attempt to login with a given username.
	AuthLockDuration int64       // Seconds which the user will be locked out if MaxAuthAttempts has been exceeded.
	PassGen          PasswordGen // A function used to generate passwords and reset tokens
	// (as opposed to registered) this is the length of the generated password length.
	UsernameIsEmail  *bool // When true (default) the username is the email address. When false the username can be specified independently. In either scenario both can be used to sign in with the password.
	ResetTokenExpiry int64 // ResetTokenExpiry Seconds before token expired.
}

type User struct {
	Id        int64  `json:"id"`
	Uid       string `json:"uid"`      // A universally unique id such as a uuid
	Username  string `json:"username"` // Same as email?? If not supplied.
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Phone     string `json:"phone"`
	OrgId     int64  `json:"org_id"`
	OrgName   string `json:"org_name"`
	Updated   int64  `json:"updated"`
	Created   int64  `json:"created"`
	Role      Role   `json:"role"`
	Passive   bool   `json:"passive"`
	Suspended bool   `json:"suspended"`
}

type UserWithClaims struct {
	*User
	*Claims
}

type Claims struct {
	Role         Role  `json:"role"`
	OrgId        int64 `json:"org_id"`
	OrgSuspended bool  `json:"org_suspended"`
}

type UserWithToken struct {
	User
	Token string `json:"token"`
}

func NewUsers(db *sql.DB, opt UserOpts) *Users {
	if opt.AuthLockDuration == 0 {
		opt.AuthLockDuration = 5 * 60
	}
	if opt.ResetTokenExpiry == 0 {
		opt.ResetTokenExpiry = 24 * 60 * 60 * 1000
	}
	if opt.PassGen == nil {
		opt.PassGen = RandStringBytesMaskImprSrc
	}
	if opt.UsernameIsEmail == nil {
		t := true
		opt.UsernameIsEmail = &t
	}
	return &Users{
		db:        db,
		Suspender: NewSuspender("users", db),
		UserOpts:  opt,
	}
}

type Users struct {
	db *sql.DB
	*Suspender
	UserOpts
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

type SignUpParams struct {
	Username        string `json:"username"`
	InviteCode      string `json:"invite_code"`
	Password        string `json:"password"`
	Email           string `json:"email"`
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name"`
	Phone           string `json:"phone"`
	OrgId           int64  `json:"org_id"`
	Role            Role   `json:"role"`
	Passive         bool   `json:"passive"`
	CustomValidator `json:"-"`
}

func (va *SignUpParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if !govalidator.IsEmail(va.Email) {
		return ErrEmailRequired
	}
	return nil
}

type ExistsParams struct {
	Email    string `json:"email"`
	Username string `json:"username"`
}

// Exists returns true only if we know for certain that the email and username don't exists, otherwise we assume they might exist or they definitely exists if the error indicates as such.
func (us *Users) Exists(p ExistsParams) (bool, error) {
	var exists bool
	err := Tx(us.db, func(tx *sql.Tx) error {
		e, err := us.exists(tx, p)
		if err != nil {
			return err
		}
		exists = e
		return nil
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (us *Users) exists(tx *sql.Tx, p ExistsParams) (bool, error) {
	existingQ, err := tx.Prepare("SELECT username, email  FROM users WHERE deleted = 0 AND username = ? OR email = ?")
	if err != nil {
		return true, err
	}

	var username, email string
	err = existingQ.QueryRow(p.Username, p.Email).Scan(&username, &email)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return true, err
	}

	if strings.ToLower(email) == strings.ToLower(p.Email) {
		return true, ErrEmailTaken
	}
	if strings.ToLower(username) == strings.ToLower(p.Username) {
		return true, ErrUsernameTaken
	}
	return false, nil
}

// SignUp returns a user, random password and [error]
func (us *Users) SignUp(p SignUpParams) (*User, string, error) {
	var givenPassword bool
	var activateToken = ""
	var id int64
	var u *User
	if p.Passive && p.Email == "" {
		p.Email = uuid.NewV4().String() + "@passive-user.gus"
	}
	err := Tx(us.db, func(tx *sql.Tx) error {
		exists, err := us.exists(tx, ExistsParams{Username: p.Username, Email: p.Email})
		if exists {
			return err
		}
		stmt, err := tx.Prepare("INSERT INTO users(" +
			"username, uid, email, first_name, " +
			"last_name, phone, password_hash, org_id, " +
			"updated, created, deleted, role, " +
			"suspended, invite_code, passive) " +
			"values(" +
			"?,?,?,?," +
			"?,?,?,?," +
			"?,?,?,?," +
			"?, ?, ?)")
		if err != nil {
			return err
		}
		if *us.UserOpts.UsernameIsEmail || p.Username == "" {
			p.Username = p.Email
		}
		u = &User{
			Uid: uuid.NewV4().String(), Username: p.Username, Email: p.Email, FirstName: p.FirstName,
			LastName: p.LastName, Phone: p.Phone, OrgId: p.OrgId, Created: Milliseconds(time.Now()),
			Updated: Milliseconds(time.Now()), Role: p.Role, Suspended: false, Passive: p.Passive}

		if p.Password == "" {
			p.Password = us.UserOpts.PassGen(128)
			if err != nil {
				return err
			}
		} else {
			givenPassword = true
		}
		hash, err := hashPassword(p.Password)
		if err != nil {
			return err
		}
		res, err := stmt.Exec(
			u.Username, u.Uid, u.Email, u.FirstName,
			u.LastName, u.Phone, hash, u.OrgId,
			u.Updated, u.Created, 0, u.Role,
			u.Suspended, p.InviteCode, p.Passive)
		if err != nil {
			return err
		}
		lid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		id = lid
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	u.Id = id
	if givenPassword {
		return u, "", nil
	}

	if !u.Passive {
		at, err := us.ResetPassword(ResetPasswordParams{Email: p.Email})
		if err != nil {
			return nil, "", err
		}
		activateToken = at
	}
	return u, activateToken, nil
}

func (us *Users) Get(id int64) (*User, error) {
	stmt, err := us.db.Prepare("SELECT id, uid, username, email, first_name, last_name, phone, org_id, created, updated, role, suspended, passive from users WHERE id =  ? AND deleted = 0 LIMIT 1")
	if err != nil {
		return nil, err
	}
	return scanUser(stmt.QueryRow(id))
}

// GetByUsername returns a user by username (or email) as well as a password hash.
func (us *Users) GetByUsername(username string) (*UserWithClaims, string, error) {
	stmt, err := us.db.Prepare("SELECT u.password_hash, u.id, u.uid, u.username, u.email, u.first_name, u.last_name, u.phone, u.org_id, u.created, u.updated, u.role, u.suspended, COALESCE(o.suspended, 0), passive from users u left join orgs o on u.org_id = o.id WHERE u.email = ? OR u.username = ? AND u.deleted = 0 LIMIT 1")
	if err != nil {
		return nil, "", err
	}
	row := stmt.QueryRow(username, username)
	var u User
	var passwordHash string
	var orgSuspended bool
	var suspended int
	var passive sql.NullBool
	err = CheckNotFound(row.Scan(&passwordHash, &u.Id, &u.Uid, &u.Username, &u.Email, &u.FirstName, &u.LastName, &u.Phone,
		&u.OrgId, &u.Created, &u.Updated, &u.Role, &suspended, &orgSuspended, &passive))
	if err != nil {
		return nil, "", err
	}
	if passive.Valid {
		u.Passive = passive.Bool
	}
	u.Suspended = suspended > 0
	c := &UserWithClaims{User: &u, Claims: &Claims{OrgId: u.OrgId, Role: u.Role, OrgSuspended: orgSuspended}}
	return c, passwordHash, err
}

type SignInParams struct {
	Email           string `json:"email"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	CustomValidator `json:"-"`
}

func (va *SignInParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if govalidator.IsNull(va.Password) {
		return ErrPasswordRequired
	}
	if !govalidator.IsEmail(va.Username) && govalidator.IsNull(va.Username) {
		return ErrUsernameOrEmailRequired
	}
	return nil
}

func (us *Users) SignIn(p SignInParams) (*UserWithClaims, error) {
	if p.Email != "" {
		if *us.UsernameIsEmail {
			p.Username = p.Email
		}
		if p.Username == "" {
			p.Username = p.Email
		}
	}
	if us.isLocked(p.Username) {
		return nil, &RateLimitExceededError{Messages: []string{"Too many sign-in attempts try again later."}}
	}
	u, hash, err := us.GetByUsername(p.Username)
	if err != nil {
		_, ok := err.(*NotFoundError)
		if ok {
			return nil, ErrNotAuth
		}
		return nil, err
	}
	if u.Suspended || u.OrgSuspended || u.Passive {
		Debug("FAILED ATTEMPT:", us.isLocked(p.Username))
		return nil, ErrNotAuth
	}
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(p.Password))
	if err != nil {
		return nil, ErrNotAuth
	}
	return u, nil
}

// isLocked will prevent users from authenticating if they have attempted to or signed in more than n times
// within the AuthLockDuration time. e.g. if the AuthLockDuration is 600 seconds and the MaxAuthAttempts is
// 5 they will be locked out when attempting to sign in immediately after the 5th attempt. Since the lock is
// 'sliding' they will not usually have to wait the full AuthLockDuration, just until there are no more than 5
// attempts in last 600 seconds. The effective sign-in rate would thus be 1 'sign in' per minute or one burst of 5
// 'sign ins' every 5 minutes.
func (us *Users) isLocked(username string) bool {
	stmt, err := us.db.Prepare("INSERT into password_attempts (username, created) values (?, ?)")
	if err != nil {
		LogErr(err)
		return true
	}
	_, err = stmt.Exec(username, Milliseconds(time.Now()))
	if err != nil {
		LogErr(err)
		// Lock the account regardless
		return true
	}

	since := (time.Now().Unix() - us.AuthLockDuration) * 1000
	row := us.db.QueryRow("SELECT COUNT(username) FROM password_attempts WHERE created > ? AND username = ?", since, username)
	var count int64
	err = row.Scan(&count)
	if err != nil {
		LogErr(err)
		// Lock the account regardless
		return true
	}
	return count > us.AuthAttempts
}

type UpdateUserParams struct {
	Id              *int64  `json:"id"`
	FirstName       *string `json:"first_name"`
	LastName        *string `json:"last_name"`
	Email           *string `json:"email"`
	Phone           *string `json:"phone"`
	CustomValidator `json:"-"`
}

func (va *UpdateUserParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if *va.Email != "" && !govalidator.IsEmail(*va.Email) {
		return ErrInvalid("'email' invalid.")
	}
	return nil
}

func (us *Users) Update(p UpdateUserParams) error {
	u, err := us.Get(*p.Id)
	if err != nil {
		return err
	}
	_ = ApplyUpdates(u, p)
	stmt, err := us.db.Prepare("UPDATE users SET first_name = ?, last_name = ?, email = ?, phone = ?, updated = ? WHERE id = ? AND deleted = 0")
	if err != nil {
		return err
	}
	err = CheckUpdated(stmt.Exec(u.FirstName, u.LastName, u.Email, u.Phone, Milliseconds(time.Now()), u.Id))
	if err != nil && strings.Contains(err.Error(), "Duplicate entry") { // ERR_STRING_EMAIL_CONSTRAINT) {
		return ErrEmailTaken
	}
	return err
}

type AssignRoleParams struct {
	Id              *int64 `json:"id"`
	Role            *Role  `json:"role"`
	CustomValidator `json:"-"`
}

func (va *AssignRoleParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if va.Role == nil {
		return ErrInvalid("A 'role' is required. Supply '0' for no permissions.")
	}
	return nil
}

func (us *Users) AssignRole(p AssignRoleParams) error {
	u, err := us.Get(*p.Id)
	if err != nil {
		return err
	}
	if u.Passive {
		return ErrInvalid("This user is passive, cannot assign a role")
	}
	stmt, err := us.db.Prepare("UPDATE users SET role = ?, updated = ? WHERE id = ? AND deleted = 0")
	if err != nil {
		return err
	}
	if p.Role == nil {
		u.Role = 0
	} else {
		u.Role = *p.Role
	}
	return CheckUpdated(stmt.Exec(u.Role, Milliseconds(time.Now()), u.Id))
}

func (us *Users) Delete(id int64) error {
	stmt, err := us.db.Prepare("UPDATE users SET deleted = 1, updated = ? WHERE id = ? AND deleted = 0")
	if err != nil {
		return err
	}
	return CheckUpdated(stmt.Exec(Milliseconds(time.Now()), id))
}

type ListUsersParams struct {
	ListArgs
	CustomValidator `json:"-"`
	UserFilters
}

type UserFilters struct {
	OrgId     int64  `schema:"org_id"` // sort by org name
	Role      int64  `schema:"role"`
	Name      string `schema:"name"` // first name
	Email     string `schema:"email"`
	Suspended *bool  `schema:"suspended"`
	Phone     string `schema:"phone"`
}

type UserListResponse struct {
	ListArgs
	Total int64   `json:"total"`
	Items []*User `json:"items"`
}

func (va *ListUsersParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	return nil
}

func (us *Users) List(p ListUsersParams) (*UserListResponse, error) {
	q := "SELECT u.id, u.uid, u.username, u.email, u.first_name, u.last_name, u.phone," +
		" u.org_id, o.name as org_name, u.created, u.updated, u.role, u.suspended, u.passive " +
		"From users u left join orgs o on u.org_id = o.id WHERE 1"
	countq := "SELECT count(u.id) FROM users u WHERE 1"

	args := []interface{}{}
	if !p.Deleted {
		q += " AND u.deleted = 0"
		countq += " AND u.deleted = 0"
	}
	if p.OrgId > 0 {
		q, countq, args = addClause(q, countq, " AND u.org_id = ?", args, p.OrgId)
	}
	if p.Role > 0 {
		q, countq, args = addClause(q, countq, " AND u.role = ?", args, p.Role)
	}
	if p.Suspended != nil {
		if *p.Suspended {
			q += " AND u.suspended = 1"
			countq += " AND u.suspended = 1"
		} else {
			q += " AND u.suspended = 0"
			countq += " AND u.suspended = 0"
		}
	}
	if p.Name != "" {
		q, countq, args = addClause(q, countq, " AND (u.first_name like ?", args, "%"+p.Name+"%")
		q, countq, args = addClause(q, countq, " OR u.last_name like ?)", args, "%"+p.Name+"%")
	}
	if p.Phone != "" {
		q, countq, args = addClause(q, countq, " AND u.phone like ?", args, "%"+p.Phone+"%")
	}
	if p.Email != "" {
		q, countq, args = addClause(q, countq, " AND u.email like ?", args, "%"+p.Email+"%")
	}
	rows, err := GetRows(us.db, q, &p.ListArgs, args...)
	if err != nil {
		return nil, err
	}
	row := us.db.QueryRow(countq, args...)
	var total int64
	err = row.Scan(&total)
	if err != nil {
		return nil, err
	}
	users := []*User{}
	for rows.Next() {
		u := &User{}
		var orgName sql.NullString
		var passive sql.NullBool
		err2 := rows.Scan(&u.Id, &u.Uid, &u.Username, &u.Email, &u.FirstName, &u.LastName, &u.Phone, &u.OrgId, &orgName, &u.Created, &u.Updated, &u.Role, &u.Suspended, &passive)
		if err2 != nil {
			return nil, err
		}
		if passive.Valid {
			u.Passive = passive.Bool
		}
		if orgName.Valid {
			u.OrgName = orgName.String
		}
		users = append(users, u)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return &UserListResponse{
		Total: total,
		Items: users,
		ListArgs: ListArgs{
			Size:      p.Size,
			Page:      p.Page,
			Direction: p.Direction,
			OrderBy:   p.OrderBy,
			Deleted:   p.Deleted,
		}}, nil
}

func addClause(sqla string, sqlb string, clause string, params []interface{}, val interface{}) (string, string, []interface{}) {
	sqla += clause
	sqlb += clause
	return sqla, sqlb, append(params, val)
}

type ResetPasswordParams struct {
	Email           string `json:"email"`
	CustomValidator `json:"-"`
}

func (va *ResetPasswordParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if govalidator.IsNull(va.Email) {
		return ErrEmailRequired
	}
	if !govalidator.IsEmail(va.Email) {
		return ErrEmailInvalid
	}
	return nil
}

func (us *Users) ResetPassword(p ResetPasswordParams) (string, error) {
	u, _, err := us.GetByUsername(p.Email)
	if err != nil {
		return "", err
	}
	if u.Passive {
		return "", ErrNotAuth
	}
	token := us.PassGen(128)
	err = Tx(us.db, func(tx *sql.Tx) error {
		_, err = tx.Exec("UPDATE password_resets set deleted = 1 where email = ?", p.Email)
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare("INSERT into password_resets (user_id, email, reset_token, created, deleted) values (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		_, err = stmt.Exec(u.Id, u.Email, token, Milliseconds(time.Now()), 0)
		if err != nil {
			LogErr(err)
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

type ChangePasswordParams struct {
	Email            string `json:"email"`
	ExistingPassword string `json:"existing_password"`
	NewPassword      string `json:"new_password"`
	ResetToken       string `json:"reset_token"`
	CustomValidator  `json:"-"`
}

func (va *ChangePasswordParams) Validate() error {
	if va.CustomValidator != nil {
		return va.CustomValidator()
	}
	if govalidator.IsNull(va.Email) {
		return ErrEmailRequired
	}
	if !govalidator.IsEmail(va.Email) {
		return ErrEmailInvalid
	}
	if govalidator.IsNull(va.ExistingPassword) && govalidator.IsNull(va.ResetToken) {
		return ErrInvalid("'existing_password' or 'reset_token' required.")
	}
	if govalidator.IsNull(va.NewPassword) {
		return ErrInvalid("'new_password' is required.")
	}
	if !ValidatePassword(va.NewPassword) {
		return ErrPasswordInvalid
	}
	return nil
}

func (us *Users) ChangePassword(p ChangePasswordParams) error {
	if p.ExistingPassword != "" {
		_, err := us.SignIn(SignInParams{Username: p.Email, Password: p.ExistingPassword})
		if err != nil {
			return err
		}
	} else if p.ResetToken != "" {
		err := Tx(us.db, func(tx *sql.Tx) error {
			stmt, err := tx.Prepare(
				"SELECT reset_token, created FROM password_resets where email = ? and  deleted = 0 " +
					"ORDER BY created DESC LIMIT 1")
			row := stmt.QueryRow(p.Email)
			var resetToken string
			var created int64
			err = CheckNotFound(row.Scan(&resetToken, &created))
			if err != nil {
				return err
			}
			if resetToken != p.ResetToken {
				return ErrInvalidResetToken
			}
			if Milliseconds(time.Now()) > (created + us.ResetTokenExpiry*1000) {
				return ErrTokenExpired
			}
			_, err = tx.Exec("UPDATE password_resets set deleted = 1 WHERE email = ?", p.Email)
			return err
		})
		if err != nil {
			return err
		}
	} else {
		return ErrNotAuth
	}
	hash, err := hashPassword(p.NewPassword)
	if err != nil {
		return err
	}
	stmt, err := us.db.Prepare("UPDATE users SET password_hash = ?, updated = ? WHERE email = ? AND deleted = 0")
	err = CheckNotFound(err)
	if err != nil {
		return err
	}
	_, err = stmt.Exec(hash, Milliseconds(time.Now()), p.Email)
	return nil
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var suspended int
	var passive sql.NullBool
	err := row.Scan(&u.Id, &u.Uid, &u.Username, &u.Email, &u.FirstName, &u.LastName, &u.Phone, &u.OrgId,
		&u.Created, &u.Updated, &u.Role, &suspended, &passive)
	u.Suspended = suspended > 0
	if passive.Valid {
		u.Passive = passive.Bool
	}
	return CheckRows(&u, err)
}

func CheckRows(u *User, e error) (*User, error) {
	if e != nil {
		if e == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, e
	}
	return u, nil
}
