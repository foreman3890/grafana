package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/grafana/grafana/pkg/api/apierrors"
	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/guardian"
	"github.com/grafana/grafana/pkg/services/libraryelements/model"
	"github.com/grafana/grafana/pkg/services/org"
	"github.com/grafana/grafana/pkg/services/search"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/web"
)

// swagger:route GET /folders folders getFolders
//
// Get all folders.
//
// Returns all folders that the authenticated user has permission to view.
// If nested folders are enabled, it expects an additional query parameter with the parent folder UID
// and returns the immediate subfolders that the authenticated user has permission to view.
// If the parameter is not supplied then it returns immediate subfolders under the root
// that the authenticated user has permission to view.
//
// Responses:
// 200: getFoldersResponse
// 401: unauthorisedError
// 403: forbiddenError
// 500: internalServerError
func (hs *HTTPServer) GetFolders(c *contextmodel.ReqContext) response.Response {
	var folders []*folder.Folder
	var err error
	if hs.Features.IsEnabled(featuremgmt.FlagNestedFolders) {
		folders, err = hs.folderService.GetChildren(c.Req.Context(), &folder.GetChildrenQuery{
			OrgID:        c.OrgID,
			Limit:        c.QueryInt64("limit"),
			Page:         c.QueryInt64("page"),
			UID:          c.Query("parentUid"),
			SignedInUser: c.SignedInUser,
		})
	} else {
		folders, err = hs.searchFolders(c)
	}

	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	result := make([]dtos.FolderSearchHit, 0)
	for _, f := range folders {
		result = append(result, dtos.FolderSearchHit{
			Id:        f.ID,
			Uid:       f.UID,
			Title:     f.Title,
			ParentUID: f.ParentUID,
		})
	}

	return response.JSON(http.StatusOK, result)
}

// swagger:route GET /folders/{folder_uid} folders getFolderByUID
//
// Get folder by uid.
//
// Responses:
// 200: folderResponse
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 500: internalServerError
func (hs *HTTPServer) GetFolderByUID(c *contextmodel.ReqContext) response.Response {
	uid := web.Params(c.Req)[":uid"]
	folder, err := hs.folderService.Get(c.Req.Context(), &folder.GetFolderQuery{OrgID: c.OrgID, UID: &uid, SignedInUser: c.SignedInUser})
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	g, err := guardian.NewByUID(c.Req.Context(), folder.UID, c.OrgID, c.SignedInUser)
	if err != nil {
		return response.Err(err)
	}

	return response.JSON(http.StatusOK, hs.newToFolderDto(c, g, folder))
}

// swagger:route GET /folders/id/{folder_id} folders getFolderByID
//
// Get folder by id.
//
// Returns the folder identified by id. This is deprecated.
// Please refer to [updated API](#/folders/getFolderByUID) instead
//
// Deprecated: true
//
// Responses:
// 200: folderResponse
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 500: internalServerError
func (hs *HTTPServer) GetFolderByID(c *contextmodel.ReqContext) response.Response {
	id, err := strconv.ParseInt(web.Params(c.Req)[":id"], 10, 64)
	if err != nil {
		return response.Error(http.StatusBadRequest, "id is invalid", err)
	}
	folder, err := hs.folderService.Get(c.Req.Context(), &folder.GetFolderQuery{ID: &id, OrgID: c.OrgID, SignedInUser: c.SignedInUser})
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	g, err := guardian.NewByUID(c.Req.Context(), folder.UID, c.OrgID, c.SignedInUser)
	if err != nil {
		return response.Err(err)
	}
	return response.JSON(http.StatusOK, hs.newToFolderDto(c, g, folder))
}

// swagger:route POST /folders folders createFolder
//
// Create folder.
//
// If nested folders are enabled then it additionally expects the parent folder UID.
//
// Responses:
// 200: folderResponse
// 400: badRequestError
// 401: unauthorisedError
// 403: forbiddenError
// 409: conflictError
// 500: internalServerError
func (hs *HTTPServer) CreateFolder(c *contextmodel.ReqContext) response.Response {
	cmd := folder.CreateFolderCommand{}
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}
	cmd.OrgID = c.OrgID
	cmd.SignedInUser = c.SignedInUser

	folder, err := hs.folderService.Create(c.Req.Context(), &cmd)
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	if err := hs.setDefaultFolderPermissions(c.Req.Context(), cmd.OrgID, cmd.SignedInUser, folder); err != nil {
		hs.log.Error("Could not set the default folder permissions", "folder", folder.Title, "user", cmd.SignedInUser, "error", err)
	}

	// Clear permission cache for the user who's created the folder, so that new permissions are fetched for their next call
	// Required for cases when caller wants to immediately interact with the newly created object
	hs.accesscontrolService.ClearUserPermissionCache(c.SignedInUser)

	g, err := guardian.NewByUID(c.Req.Context(), folder.UID, c.OrgID, c.SignedInUser)
	if err != nil {
		return response.Err(err)
	}

	// TODO set ParentUID if nested folders are enabled
	return response.JSON(http.StatusOK, hs.newToFolderDto(c, g, folder))
}

func (hs *HTTPServer) setDefaultFolderPermissions(ctx context.Context, orgID int64, user *user.SignedInUser, folder *folder.Folder) error {
	var permissions []accesscontrol.SetResourcePermissionCommand
	if user.IsRealUser() && !user.IsAnonymous {
		permissions = append(permissions, accesscontrol.SetResourcePermissionCommand{
			UserID: user.UserID, Permission: dashboards.PERMISSION_ADMIN.String(),
		})
	}

	isNested := folder.ParentUID != ""
	if !isNested || !hs.Features.IsEnabled(featuremgmt.FlagNestedFolders) {
		permissions = append(permissions, []accesscontrol.SetResourcePermissionCommand{
			{BuiltinRole: string(org.RoleEditor), Permission: dashboards.PERMISSION_EDIT.String()},
			{BuiltinRole: string(org.RoleViewer), Permission: dashboards.PERMISSION_VIEW.String()},
		}...)
	}

	_, err := hs.folderPermissionsService.SetPermissions(ctx, orgID, folder.UID, permissions...)
	return err
}

// swagger:route POST /folders/{folder_uid}/move folders moveFolder
//
// Move folder.
//
// Responses:
// 200: folderResponse
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 500: internalServerError
func (hs *HTTPServer) MoveFolder(c *contextmodel.ReqContext) response.Response {
	if hs.Features.IsEnabled(featuremgmt.FlagNestedFolders) {
		cmd := folder.MoveFolderCommand{}
		if err := web.Bind(c.Req, &cmd); err != nil {
			return response.Error(http.StatusBadRequest, "bad request data", err)
		}
		var err error

		cmd.OrgID = c.OrgID
		cmd.UID = web.Params(c.Req)[":uid"]
		cmd.SignedInUser = c.SignedInUser
		theFolder, err := hs.folderService.Move(c.Req.Context(), &cmd)
		if err != nil {
			return response.Error(http.StatusInternalServerError, "move folder failed", err)
		}

		g, err := guardian.NewByUID(c.Req.Context(), cmd.UID, c.OrgID, c.SignedInUser)
		if err != nil {
			return response.Err(err)
		}
		return response.JSON(http.StatusOK, hs.newToFolderDto(c, g, theFolder))
	}
	result := map[string]string{}
	result["message"] = "To use this service, you need to activate nested folder feature."
	return response.JSON(http.StatusNotFound, result)
}

// swagger:route PUT /folders/{folder_uid} folders updateFolder
//
// Update folder.
//
// Responses:
// 200: folderResponse
// 400: badRequestError
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 409: conflictError
// 500: internalServerError
func (hs *HTTPServer) UpdateFolder(c *contextmodel.ReqContext) response.Response {
	cmd := folder.UpdateFolderCommand{}
	if err := web.Bind(c.Req, &cmd); err != nil {
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	cmd.OrgID = c.OrgID
	cmd.UID = web.Params(c.Req)[":uid"]
	cmd.SignedInUser = c.SignedInUser
	result, err := hs.folderService.Update(c.Req.Context(), &cmd)
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}
	g, err := guardian.NewByUID(c.Req.Context(), result.UID, c.OrgID, c.SignedInUser)
	if err != nil {
		return response.Err(err)
	}

	return response.JSON(http.StatusOK, hs.newToFolderDto(c, g, result))
}

// swagger:route DELETE /folders/{folder_uid} folders deleteFolder
//
// Delete folder.
//
// Deletes an existing folder identified by UID along with all dashboards (and their alerts) stored in the folder. This operation cannot be reverted.
// If nested folders are enabled then it also deletes all the subfolders.
//
// Responses:
// 200: deleteFolderResponse
// 400: badRequestError
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 500: internalServerError
func (hs *HTTPServer) DeleteFolder(c *contextmodel.ReqContext) response.Response { // temporarily adding this function to HTTPServer, will be removed from HTTPServer when librarypanels featuretoggle is removed
	err := hs.LibraryElementService.DeleteLibraryElementsInFolder(c.Req.Context(), c.SignedInUser, web.Params(c.Req)[":uid"])
	if err != nil {
		if errors.Is(err, model.ErrFolderHasConnectedLibraryElements) {
			return response.Error(403, "Folder could not be deleted because it contains library elements in use", err)
		}
		return apierrors.ToFolderErrorResponse(err)
	}

	uid := web.Params(c.Req)[":uid"]
	err = hs.folderService.Delete(c.Req.Context(), &folder.DeleteFolderCommand{UID: uid, OrgID: c.OrgID, ForceDeleteRules: c.QueryBool("forceDeleteRules"), SignedInUser: c.SignedInUser})
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	return response.JSON(http.StatusOK, "")
}

// swagger:route GET /folders/{folder_uid}/counts folders getFolderDescendantCounts
//
// Gets the count of each descendant of a folder by kind. The folder is identified by UID.
//
// Responses:
// 200: getFolderDescendantCountsResponse
// 401: unauthorisedError
// 403: forbiddenError
// 404: notFoundError
// 500: internalServerError
func (hs *HTTPServer) GetFolderDescendantCounts(c *contextmodel.ReqContext) response.Response {
	uid := web.Params(c.Req)[":uid"]
	counts, err := hs.folderService.GetDescendantCounts(c.Req.Context(), &folder.GetDescendantCountsQuery{OrgID: c.OrgID, UID: &uid, SignedInUser: c.SignedInUser})
	if err != nil {
		return apierrors.ToFolderErrorResponse(err)
	}

	return response.JSON(http.StatusOK, counts)
}
func (hs *HTTPServer) newToFolderDto(c *contextmodel.ReqContext, g guardian.DashboardGuardian, f *folder.Folder) dtos.Folder {
	ctx := c.Req.Context()
	toDTO := func(f *folder.Folder) dtos.Folder {
		canEdit, _ := g.CanEdit()
		canSave, _ := g.CanSave()
		canAdmin, _ := g.CanAdmin()
		canDelete, _ := g.CanDelete()

		// Finding creator and last updater of the folder
		updater, creator := anonString, anonString
		if f.CreatedBy > 0 {
			creator = hs.getUserLogin(ctx, f.CreatedBy)
		}
		if f.UpdatedBy > 0 {
			updater = hs.getUserLogin(ctx, f.UpdatedBy)
		}

		acMetadata, _ := hs.getFolderACMetadata(c, f)

		return dtos.Folder{
			Id:            f.ID,
			Uid:           f.UID,
			Title:         f.Title,
			Url:           f.URL,
			HasACL:        f.HasACL,
			CanSave:       canSave,
			CanEdit:       canEdit,
			CanAdmin:      canAdmin,
			CanDelete:     canDelete,
			CreatedBy:     creator,
			Created:       f.Created,
			UpdatedBy:     updater,
			Updated:       f.Updated,
			Version:       f.Version,
			AccessControl: acMetadata,
			ParentUID:     f.ParentUID,
		}
	}

	folderDTO := toDTO(f)

	if !hs.Features.IsEnabled(featuremgmt.FlagNestedFolders) {
		return folderDTO
	}

	parents, err := hs.folderService.GetParents(ctx, folder.GetParentsQuery{UID: f.UID, OrgID: f.OrgID})
	if err != nil {
		// log the error instead of failing
		hs.log.Error("failed to fetch folder parents", "folder", f.UID, "org", f.OrgID, "error", err)
	}

	folderDTO.Parents = make([]dtos.Folder, 0, len(parents))
	for _, f := range parents {
		folderDTO.Parents = append(folderDTO.Parents, toDTO(f))
	}

	return folderDTO
}

func (hs *HTTPServer) getFolderACMetadata(c *contextmodel.ReqContext, f *folder.Folder) (accesscontrol.Metadata, error) {
	if !c.QueryBool("accesscontrol") {
		return nil, nil
	}

	parents, err := hs.folderService.GetParents(c.Req.Context(), folder.GetParentsQuery{UID: f.UID, OrgID: c.OrgID})
	if err != nil {
		return nil, err
	}

	folderIDs := map[string]bool{f.UID: true}
	for _, p := range parents {
		folderIDs[p.UID] = true
	}

	allMetadata := hs.getMultiAccessControlMetadata(c, c.OrgID, dashboards.ScopeFoldersPrefix, folderIDs)
	metadata := allMetadata[f.UID]

	// Flatten metadata - if any parent has a permission, the child folder inherits it
	for _, md := range allMetadata {
		for action := range md {
			metadata[action] = true
		}
	}
	return metadata, nil
}

func (hs *HTTPServer) searchFolders(c *contextmodel.ReqContext) ([]*folder.Folder, error) {
	searchQuery := search.Query{
		SignedInUser: c.SignedInUser,
		DashboardIds: make([]int64, 0),
		FolderIds:    make([]int64, 0),
		Limit:        c.QueryInt64("limit"),
		OrgId:        c.OrgID,
		Type:         "dash-folder",
		Permission:   dashboards.PERMISSION_VIEW,
		Page:         c.QueryInt64("page"),
	}

	hits, err := hs.SearchService.SearchHandler(c.Req.Context(), &searchQuery)
	if err != nil {
		return nil, err
	}

	folders := make([]*folder.Folder, 0)

	for _, hit := range hits {
		folders = append(folders, &folder.Folder{
			ID:    hit.ID,
			UID:   hit.UID,
			Title: hit.Title,
		})
	}

	return folders, nil
}

// swagger:parameters getFolders
type GetFoldersParams struct {
	// Limit the maximum number of folders to return
	// in:query
	// required:false
	// default:1000
	Limit int64 `json:"limit"`
	// Page index for starting fetching folders
	// in:query
	// required:false
	// default:1
	Page int64 `json:"page"`
	// The parent folder UID
	// in:query
	// required:false
	ParentUID string `json:"parentUid"`
}

// swagger:parameters getFolderByUID
type GetFolderByUIDParams struct {
	// in:path
	// required:true
	FolderUID string `json:"folder_uid"`
}

// swagger:parameters updateFolder
type UpdateFolderParams struct {
	// in:path
	// required:true
	FolderUID string `json:"folder_uid"`
	// To change the unique identifier (uid), provide another one.
	// To overwrite an existing folder with newer version, set `overwrite` to `true`.
	// Provide the current version to safelly update the folder: if the provided version differs from the stored one the request will fail, unless `overwrite` is `true`.
	//
	// in:body
	// required:true
	Body folder.UpdateFolderCommand `json:"body"`
}

// swagger:parameters getFolderByID
type GetFolderByIDParams struct {
	// in:path
	// required:true
	FolderID int64 `json:"folder_id"`
}

// swagger:parameters createFolder
type CreateFolderParams struct {
	// in:body
	// required:true
	Body folder.CreateFolderCommand `json:"body"`
}

// swagger:parameters moveFolder
type MoveFolderParams struct {
	// in:path
	// required:true
	FolderUID string `json:"folder_uid"`
	// in:body
	// required:true
	Body folder.MoveFolderCommand `json:"body"`
}

// swagger:parameters deleteFolder
type DeleteFolderParams struct {
	// in:path
	// required:true
	FolderUID string `json:"folder_uid"`
	// If `true` any Grafana 8 Alerts under this folder will be deleted.
	// Set to `false` so that the request will fail if the folder contains any Grafana 8 Alerts.
	// in:query
	// required:false
	// default:false
	ForceDeleteRules bool `json:"forceDeleteRules"`
}

// swagger:response getFoldersResponse
type GetFoldersResponse struct {
	// The response message
	// in: body
	Body []dtos.FolderSearchHit `json:"body"`
}

// swagger:response folderResponse
type FolderResponse struct {
	// The response message
	// in: body
	Body dtos.Folder `json:"body"`
}

// swagger:response deleteFolderResponse
type DeleteFolderResponse struct {
	// The response message
	// in: body
	Body struct {
		// ID Identifier of the deleted folder.
		// required: true
		// example: 65
		ID int64 `json:"id"`

		// Title of the deleted folder.
		// required: true
		// example: My Folder
		Title string `json:"title"`

		// Message Message of the deleted folder.
		// required: true
		// example: Folder My Folder deleted
		Message string `json:"message"`
	} `json:"body"`
}

// swagger:parameters getFolderDescendantCounts
type GetFolderDescendantCountsParams struct {
	// in:path
	// required:true
	FolderUID string `json:"folder_uid"`
}

// swagger:response getFolderDescendantCountsResponse
type GetFolderDescendantCountsResponse struct {
	// The response message
	// in: body
	Body folder.DescendantCounts `json:"body"`
}
