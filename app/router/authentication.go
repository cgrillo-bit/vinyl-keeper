package router

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ninesl/vinyl-keeper/app/auth"
	"github.com/ninesl/vinyl-keeper/app/router/assets/ui"
	"github.com/ninesl/vinyl-keeper/app/router/assets/ui/parts"
	"github.com/ninesl/vinyl-keeper/app/router/values"
	"github.com/ninesl/vinyl-keeper/app/vinyl"
)

// ErrAuthMiddlewareNotRun indicates the auth middleware did not execute
var ErrAuthMiddlewareNotRun = errors.New("auth middleware did not run")

// determineUser extracts the authenticated user from the request context
// Returns nil if not authenticated (anonymous user)
// Returns error if auth middleware didn't run (programming error - should be 500)
func determineUser(r *http.Request) (*vinyl.User, error) {
	user, ok := GetUserFromContext(r.Context())
	if !ok {
		// Middleware didn't run - this is a programming error
		return nil, ErrAuthMiddlewareNotRun
	}
	return user, nil
}

// GetUserID extracts the user ID from the context, returns -1 if not authenticated
func GetUserID(r *http.Request) int64 {
	user, _ := determineUser(r)
	if user == nil {
		return -1
	}
	return user.UserID
}

// GetUserName extracts the username from the context, returns empty string if not authenticated
func GetUserName(r *http.Request) string {
	user, _ := determineUser(r)
	if user == nil {
		return ""
	}
	return user.UserName
}

// IsUserSignedIn checks if a user is currently signed in
func IsUserSignedIn(r *http.Request) bool {
	return GetUserID(r) >= 0
}

// SignInUsersListHandler returns the list of users for the sign-in modal
func SignInUsersListHandler(k UserLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		users, err := k.ListUsers()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to load users").Render(r.Context(), w)
			return
		}

		signedInID := GetUserID(r)
		ui.SignInUsersList(users, signedInID).Render(r.Context(), w)
	}
}

// SignInButtonHandler returns the sign-in button UI (shows username if signed in)
func SignInButtonHandler(k UserGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		signedInID := GetUserID(r)
		if signedInID < 0 {
			ui.SignInButtonZone("").Render(r.Context(), w)
			return
		}

		user, err := k.GetUserByID(signedInID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to load signed-in user").Render(r.Context(), w)
			return
		}

		if user == nil {
			ui.SignInButtonZone("").Render(r.Context(), w)
			return
		}

		ui.SignInButtonZone(user.UserName).Render(r.Context(), w)
	}
}

// SignInCreateUserHandler creates a new user and returns the refreshed users list.
func SignInCreateUserHandler(k UserCreatorLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		name := r.FormValue(values.QueryUserName)
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Missing user name").Render(r.Context(), w)
			return
		}

		_, err := k.CreateUser(name)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Could not create user (name may already exist)").Render(r.Context(), w)
			return
		}

		users, err := k.ListUsers()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to load users").Render(r.Context(), w)
			return
		}

		signedInID := GetUserID(r)
		ui.SignInUsersList(users, signedInID).Render(r.Context(), w)
	}
}

// SignInDeleteUserHandler deletes a user and returns the refreshed users list.
func SignInDeleteUserHandler(k UserDeleterLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		userIDStr := r.FormValue(values.QueryUserID)
		if userIDStr == "" {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Missing user ID").Render(r.Context(), w)
			return
		}

		userID, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil || userID < 0 {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Invalid user ID").Render(r.Context(), w)
			return
		}

		signedInID := GetUserID(r)
		deletingSignedInUser := signedInID == userID

		if err := k.DeleteUser(userID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to delete user").Render(r.Context(), w)
			return
		}

		if deletingSignedInUser {
			auth.ClearSessionCookie(w)
			w.Header().Set(values.HeaderHXTriggerAfterSettle, values.EventUserSignedOut)
			ui.SignInPanel("", true).Render(r.Context(), w)
			return
		}

		users, err := k.ListUsers()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to load users").Render(r.Context(), w)
			return
		}

		ui.SignInUsersList(users, signedInID).Render(r.Context(), w)
	}
}

// SignInSubmitHandler handles the actual sign-in (creates encrypted JWT session)
func SignInSubmitHandler(k UserGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		userIDStr := r.FormValue(values.QueryUserID)
		if userIDStr == "" {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Missing user ID").Render(r.Context(), w)
			return
		}

		userID, err := strconv.ParseInt(userIDStr, 10, 64)
		if err != nil || userID < 0 {
			w.WriteHeader(http.StatusBadRequest)
			parts.ErrorMessage("Invalid user ID").Render(r.Context(), w)
			return
		}

		user, err := k.GetUserByID(userID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to sign in").Render(r.Context(), w)
			return
		}
		if user == nil {
			w.WriteHeader(http.StatusNotFound)
			parts.ErrorMessage("User not found").Render(r.Context(), w)
			return
		}

		// Create encrypted JWT session cookie
		if err := auth.CreateSessionCookie(w, user.UserID, user.UserName); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			parts.ErrorMessage("Failed to create session").Render(r.Context(), w)
			return
		}

		// Trigger HTMX event AFTER settle so cookie is fully processed
		w.Header().Set(values.HeaderHXTriggerAfterSettle, values.EventUserSignedIn)
		w.WriteHeader(http.StatusOK)

		// Refresh the sign-in panel to show the signed-in state
		// oob=false because we're doing a regular swap into the modal content
		ui.SignInPanel(user.UserName, false).Render(r.Context(), w)
	}
}

// SignOutHandler handles sign-out (clears the session cookie)
func SignOutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		// Clear the session cookie
		auth.ClearSessionCookie(w)

		// Trigger HTMX event AFTER settle so cookie is fully cleared
		w.Header().Set(values.HeaderHXTriggerAfterSettle, values.EventUserSignedOut)
		w.WriteHeader(http.StatusOK)

		// Return updated sign-in panel (anonymous)
		ui.SignInPanel("", false).Render(r.Context(), w)
	}
}

// SignInPanelHandler renders the sign-in panel with current user info
func SignInPanelHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)

		ui.SignInPanel(GetUserName(r), false).Render(r.Context(), w)
	}
}

func SignInBootstrapHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if IsUserSignedIn(r) {
			w.Header().Set(values.HeaderHXTriggerAfterSettle, values.EventUserSignedIn)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// NavAuthButtonsHandler returns the auth-dependent nav buttons (My Collection, Register)
func NavAuthButtonsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", values.ContentTypeHTML)
		ui.NavAuthButtons(IsUserSignedIn(r)).Render(r.Context(), w)
	}
}

// Interfaces for dependency injection
type UserLister interface {
	ListUsers() ([]vinyl.User, error)
}

type UserGetter interface {
	GetUserByID(userID int64) (*vinyl.User, error)
}

type UserCreator interface {
	CreateUser(name string) (vinyl.User, error)
}

type UserDeleter interface {
	DeleteUser(userID int64) error
}

type UserCreatorLister interface {
	UserCreator
	UserLister
}

type UserDeleterLister interface {
	UserDeleter
	UserLister
}
