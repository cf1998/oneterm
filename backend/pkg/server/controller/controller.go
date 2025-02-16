package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/spf13/cast"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"

	"github.com/veops/oneterm/pkg/server/auth/acl"
	"github.com/veops/oneterm/pkg/server/model"
	"github.com/veops/oneterm/pkg/server/remote"
	"github.com/veops/oneterm/pkg/server/storage/db/mysql"
)

var (
	defaultHttpResponse = &HttpResponse{
		Code:    0,
		Message: "ok",
		Data:    nil,
	}
)

type preHook[T any] func(*gin.Context, T)
type postHook[T any] func(*gin.Context, []T)
type deleteCheck func(*gin.Context, int)

type Controller struct{}

func NewController() *Controller {
	return &Controller{}
}

type HttpResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type ListData struct {
	Count int64 `json:"count"`
	List  []any `json:"list"`
}

func NewHttpResponseWithData(data any) *HttpResponse {
	return &HttpResponse{
		Code:    0,
		Message: "ok",
		Data:    data,
	}
}

func doCreate[T model.Model](ctx *gin.Context, needAcl bool, md T, resourceType string, preHooks ...preHook[T]) (err error) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)

	if err = ctx.BindJSON(md); err != nil {
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInvalidArgument, Data: map[string]any{"err": err}})
		return
	}

	for _, hook := range preHooks {
		if hook == nil {
			continue
		}
		hook(ctx, md)
		if ctx.IsAborted() {
			return
		}
	}

	resourceId := 0
	if needAcl {
		if !acl.IsAdmin(currentUser) {
			ctx.AbortWithError(http.StatusForbidden, &ApiError{Code: ErrNoPerm, Data: map[string]any{"perm": acl.WRITE}})
			return
		}
		resourceId, err = acl.CreateGrantAcl(ctx, currentUser, resourceType, md.GetName())
		if err != nil {
			handleRemoteErr(ctx, err)
			return
		}
		md.SetResourceId(resourceId)
	}

	md.SetCreatorId(currentUser.Uid)
	md.SetUpdaterId(currentUser.Uid)

	if err = mysql.DB.Transaction(func(tx *gorm.DB) (err error) {
		if err = tx.Model(md).Create(md).Error; err != nil {
			return
		}

		switch t := any(md).(type) {
		case *model.Asset:
			if err = HandleAuthorization(currentUser, tx, model.ACTION_CREATE, nil, t); err != nil {
				handleRemoteErr(ctx, err)
				return
			}
		}

		if err = tx.Create(&model.History{
			RemoteIp:   ctx.ClientIP(),
			Type:       md.TableName(),
			TargetId:   md.GetId(),
			ActionType: model.ACTION_CREATE,
			Old:        nil,
			New:        toMap(md),
			CreatorId:  currentUser.Uid,
			CreatedAt:  time.Now(),
		}).Error; err != nil {
			return
		}

		return
	}); err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}

	ctx.JSON(http.StatusOK, defaultHttpResponse)

	return
}

func doDelete[T model.Model](ctx *gin.Context, needAcl bool, md T, dcs ...deleteCheck) (err error) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)
	id, err := cast.ToIntE(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInvalidArgument, Data: map[string]any{"err": err}})
		return
	}

	if err = mysql.DB.Model(md).Where("id = ?", id).First(md).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			ctx.JSON(http.StatusOK, defaultHttpResponse)
			return
		}
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}
	if needAcl {
		if !acl.IsAdmin(currentUser) && acl.HasPerm(md.GetResourceId(), currentUser.Acl.Rid, acl.DELETE) {
			ctx.AbortWithError(http.StatusForbidden, &ApiError{Code: ErrNoPerm, Data: map[string]any{"perm": acl.DELETE}})
			return
		}
	}
	for _, dc := range dcs {
		if dc == nil {
			continue
		}
		dc(ctx, id)
		if ctx.IsAborted() {
			return
		}
	}
	if needAcl {
		if err = acl.DeleteResource(ctx, currentUser.GetUid(), md.GetResourceId()); err != nil {
			handleRemoteErr(ctx, err)
			return
		}
	}

	if err = mysql.DB.Transaction(func(tx *gorm.DB) (err error) {
		switch t := any(md).(type) {
		case *model.Asset:
			if err = HandleAuthorization(currentUser, tx, model.ACTION_DELETE, t, nil); err != nil {
				handleRemoteErr(ctx, err)
				return
			}
		}

		if err = tx.Delete(md, id).Error; err != nil {
			return
		}
		err = tx.Create(&model.History{
			RemoteIp:   ctx.ClientIP(),
			Type:       md.TableName(),
			TargetId:   md.GetId(),
			ActionType: model.ACTION_DELETE,
			Old:        toMap(md),
			New:        nil,
			CreatorId:  currentUser.Uid,
			CreatedAt:  time.Now(),
		}).Error
		return
	}); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrDuplicateName, Data: map[string]any{"err": err}})
			return
		}
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}

	ctx.JSON(http.StatusOK, defaultHttpResponse)

	return
}

func doUpdate[T model.Model](ctx *gin.Context, needAcl bool, md T, preHooks ...preHook[T]) (err error) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)

	id, err := cast.ToIntE(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInvalidArgument, Data: map[string]any{"err": err}})
		return
	}

	if err = ctx.BindJSON(md); err != nil {
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInvalidArgument, Data: map[string]any{"err": err}})
		return
	}
	md.SetUpdaterId(currentUser.Uid)

	for _, hook := range preHooks {
		if hook == nil {
			continue
		}
		hook(ctx, md)
		if ctx.IsAborted() {
			return
		}
	}

	old := getEmpty(md)
	if err = mysql.DB.Model(md).Where("id = ?", id).First(old).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			ctx.JSON(http.StatusOK, defaultHttpResponse)
			return
		}
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}
	if needAcl {
		if !acl.IsAdmin(currentUser) && acl.HasPerm(md.GetResourceId(), currentUser.Acl.Rid, acl.WRITE) {
			ctx.AbortWithError(http.StatusForbidden, &ApiError{Code: ErrNoPerm, Data: map[string]any{"perm": acl.WRITE}})
			return
		}

		if err = acl.UpdateResource(ctx, currentUser.GetUid(), old.GetResourceId(), map[string]string{"name": md.GetName()}); err != nil {
			handleRemoteErr(ctx, err)
			return
		}
	}
	md.SetId(id)

	if err = mysql.DB.Transaction(func(tx *gorm.DB) (err error) {
		omits := []string{"resource_id", "created_at", "deleted_at"}
		switch t := any(md).(type) {
		case *model.Asset:
			if err = HandleAuthorization(currentUser, tx, model.ACTION_UPDATE, any(old).(*model.Asset), t); err != nil {
				handleRemoteErr(ctx, err)
				return
			}
			omits = append(omits, "ci_id")
		}

		if err = mysql.DB.Omit(omits...).Save(md).Error; err != nil {
			return
		}
		err = mysql.DB.Create(&model.History{
			RemoteIp:   ctx.ClientIP(),
			Type:       md.TableName(),
			TargetId:   md.GetId(),
			ActionType: model.ACTION_UPDATE,
			Old:        toMap(old),
			New:        toMap(md),
			CreatorId:  currentUser.Uid,
			CreatedAt:  time.Now(),
		}).Error
		return
	}); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrDuplicateName, Data: map[string]any{"err": err}})
			return
		}
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}

	ctx.JSON(http.StatusOK, defaultHttpResponse)

	return
}

func doGet[T any](ctx *gin.Context, needAcl bool, dbFind *gorm.DB, resourceType string, postHooks ...postHook[T]) (err error) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)

	if needAcl && !acl.IsAdmin(currentUser) {
		//rs := make([]*acl.Resource, 0)
		var rs []*acl.Resource
		rs, err = acl.GetRoleResources(ctx, currentUser.Acl.Rid, resourceType)
		if err != nil {
			handleRemoteErr(ctx, err)
			return
		}
		dbFind = dbFind.Where("resource_id IN ?", lo.Map(rs, func(r *acl.Resource, _ int) int { return r.ResourceId }))
	}

	dbCount := dbFind.Session(&gorm.Session{})
	dbFind = dbFind.Session(&gorm.Session{})

	count, list := int64(0), make([]T, 0)
	eg := &errgroup.Group{}
	eg.Go(func() error {
		return dbCount.Count(&count).
			Error
	})
	eg.Go(func() error {
		pi, ps := cast.ToInt(ctx.Query("page_index")), cast.ToInt(ctx.Query("page_size"))
		if _, ok := ctx.GetQuery("page_index"); ok {
			dbFind = dbFind.Offset((pi - 1) * ps)
		}
		if _, ok := ctx.GetQuery("page_size"); ok {
			dbFind = dbFind.Limit(ps)
		}
		return dbFind.
			Order("id DESC").
			Find(&list).
			Error
	})
	if err = eg.Wait(); err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
		return
	}

	for _, hook := range postHooks {
		if hook == nil {
			continue
		}
		hook(ctx, list)
		if ctx.IsAborted() {
			return
		}
	}

	res := &ListData{
		Count: count,
		List:  lo.Map(list, func(t T, _ int) any { return t }),
	}

	ctx.JSON(http.StatusOK, NewHttpResponseWithData(res))

	return
}

func handleRemoteErr(ctx *gin.Context, err error) {
	switch e := err.(type) {
	case *remote.RemoteError:
		if e.HttpCode == http.StatusBadRequest {
			ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrRemoteClient, Data: e.Resp})
			return
		}
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrRemoteServer, Data: e.Resp})
	default:
		ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
	}
}

func filterSearch(ctx *gin.Context, db *gorm.DB, fields ...string) *gorm.DB {
	q, ok := ctx.GetQuery("search")
	if !ok || len(fields) <= 0 {
		return db
	}

	d := mysql.DB
	for _, f := range fields {
		d = d.Or(fmt.Sprintf("%s LIKE ?", f), fmt.Sprintf("%%%s%%", q))
	}

	db = db.Where(d)

	return db
}
func filterStartEnd(ctx *gin.Context, db *gorm.DB, fields ...string) (*gorm.DB, error) {
	if q, ok := ctx.GetQuery("start"); ok {
		t, err := time.Parse(time.RFC3339, q)
		if err != nil {
			ctx.AbortWithError(http.StatusBadRequest, err)
			return db, err
		}
		db = db.Where("created_at >= ?", t)
	}
	if q, ok := ctx.GetQuery("end"); ok {
		t, err := time.Parse(time.RFC3339, q)
		if err != nil {
			ctx.AbortWithError(http.StatusBadRequest, err)
			return db, err
		}
		db = db.Where("created_at <= ?", t)
	}

	return db, nil
}
func filterEqual(ctx *gin.Context, db *gorm.DB, fields ...string) *gorm.DB {
	for _, f := range fields {
		if q, ok := ctx.GetQuery(f); ok {
			db = db.Where(fmt.Sprintf("%s = ?", f), q)
		}
	}

	return db
}
func filterLike(ctx *gin.Context, db *gorm.DB, fields ...string) *gorm.DB {
	likes := false
	d := mysql.DB
	for _, f := range fields {
		if q, ok := ctx.GetQuery(f); ok && q != "" {
			d = d.Or(fmt.Sprintf("%s LIKE ?", f), fmt.Sprintf("%%%s%%", q))
			likes = true
		}
	}
	if !likes {
		return db
	}
	db = db.Where(d)

	return db
}

func toMap(data any) model.Map[string, any] {
	bs, _ := json.Marshal(data)
	res := make(map[string]any)
	json.Unmarshal(bs, &res)
	return res
}

func getEmpty[T model.Model](data T) T {
	t := reflect.TypeOf(data).Elem()
	v := reflect.New(t)
	return v.Interface().(T)
}
