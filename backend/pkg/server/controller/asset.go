package controller

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/spf13/cast"
	"go.uber.org/zap"

	"github.com/veops/oneterm/pkg/conf"
	"github.com/veops/oneterm/pkg/logger"
	"github.com/veops/oneterm/pkg/server/auth/acl"
	"github.com/veops/oneterm/pkg/server/model"
	"github.com/veops/oneterm/pkg/server/schedule/connectable"
	"github.com/veops/oneterm/pkg/server/storage/db/mysql"
)

var (
	assetPostHooks = []postHook[*model.Asset]{assetPostHookCount, assetPostHookAuth}
)

// CreateAsset godoc
//
//	@Tags		asset
//	@Param		asset	body		model.Asset	true	"asset"
//	@Success	200		{object}	HttpResponse
//	@Router		/asset [post]
func (c *Controller) CreateAsset(ctx *gin.Context) {
	asset := &model.Asset{}
	doCreate(ctx, true, asset, conf.RESOURCE_ASSET)
	go connectable.CheckUpdate(asset.Id)
}

// DeleteAsset godoc
//
//	@Tags		asset
//	@Param		id	path		int	true	"asset id"
//	@Success	200	{object}	HttpResponse
//	@Router		/asset/:id [delete]
func (c *Controller) DeleteAsset(ctx *gin.Context) {
	doDelete(ctx, true, &model.Asset{})
}

// UpdateAsset godoc
//
//	@Tags		asset
//	@Param		id		path		int			true	"asset id"
//	@Param		asset	body		model.Asset	true	"asset"
//	@Success	200		{object}	HttpResponse
//	@Router		/asset/:id [put]
func (c *Controller) UpdateAsset(ctx *gin.Context) {
	doUpdate(ctx, true, &model.Asset{})
	go connectable.CheckUpdate(cast.ToInt(ctx.Param("id")))
}

// GetAssets godoc
//
//	@Tags		asset
//	@Param		page_index	query		int		true	"page_index"
//	@Param		page_size	query		int		true	"page_size"
//	@Param		search		query		string	false	"name or ip"
//	@Param		id			query		int		false	"asset id"
//	@Param		ids			query		string	false	"asset ids"
//	@Param		parent_id	query		int		false	"asset's parent id"
//	@Param		name		query		string	false	"asset name"
//	@Param		ip			query		string	false	"asset ip"
//	@Param		info		query		bool	false	"is info mode"
//	@Success	200			{object}	HttpResponse{data=ListData{list=[]model.Asset}}
//	@Router		/asset [get]
func (c *Controller) GetAssets(ctx *gin.Context) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)
	info := cast.ToBool(ctx.Query("info"))

	db := mysql.DB.Model(&model.Asset{})
	db = filterEqual(ctx, db, "id")
	db = filterLike(ctx, db, "name", "ip")
	db = filterSearch(ctx, db, "name", "ip")
	if q, ok := ctx.GetQuery("ids"); ok {
		db = db.Where("id IN ?", lo.Map(strings.Split(q, ","), func(s string, _ int) int { return cast.ToInt(s) }))
	}
	if q, ok := ctx.GetQuery("parent_id"); ok {
		parentIds, err := handleParentId(cast.ToInt(q))
		if err != nil {
			logger.L.Error("parent id found failed", zap.Error(err))
			return
		}
		db = db.Where("parent_id IN ?", parentIds)
	}

	if info && !acl.IsAdmin(currentUser) {
		//rs := make([]*acl.Resource, 0)
		rs, err := acl.GetRoleResources(ctx, currentUser.Acl.Rid, acl.GetResourceTypeName(conf.RESOURCE_AUTHORIZATION))
		if err != nil {
			handleRemoteErr(ctx, err)
			return
		}
		ids := make([]int, 0)
		if err = mysql.DB.
			Model(&model.Authorization{}).
			Where("resource_id IN ?", lo.Map(rs, func(r *acl.Resource, _ int) int { return r.ResourceId })).
			Distinct().
			Pluck("asset_id", &ids).
			Error; err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
			return
		}

		db = db.Where("id IN ?", ids)
	}

	db = db.Order("name")

	doGet[*model.Asset](ctx, !info, db, acl.GetResourceTypeName(conf.RESOURCE_AUTHORIZATION), assetPostHooks...)
}

// QueryByServer godoc
//
//	@Tags		asset
//	@Param		page_index	query		int	true	"page index"
//	@Param		page_size	query		int	true	"page size"
//	@Success	200			{object}	HttpResponse{data=ListData{list=[]model.Asset}}
//	@Router		/asset/query_by_server [get]
func (c *Controller) QueryByServer(ctx *gin.Context) {
	db := mysql.DB.Model(&model.Asset{})

	doGet[*model.Asset](ctx, false, db, acl.GetResourceTypeName(conf.RESOURCE_ASSET), nil)
}

// UpdateByServer godoc
//
//	@Tags		asset
//	@Param		id	path		int						true	"asset id"
//	@Param		req	body		map[int]map[string]any	true	"asset update request"
//	@Success	200	{object}	HttpResponse
//	@Router		/asset/update_by_server [put]
func (c *Controller) UpdateByServer(ctx *gin.Context) {
	updates := make(map[int]map[string]any)
	if err := ctx.BindJSON(&updates); err != nil {
		ctx.AbortWithError(http.StatusBadRequest, &ApiError{Code: ErrInvalidArgument, Data: map[string]any{"err": err}})
		return
	}

	for k, v := range updates {
		if err := mysql.DB.
			Model(&model.Asset{}).
			Where("id = ?", k).
			Updates(v).
			Error; err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, &ApiError{Code: ErrInternal, Data: map[string]any{"err": err}})
			return
		}
	}

	ctx.JSON(http.StatusOK, defaultHttpResponse)
}

func assetPostHookCount(ctx *gin.Context, data []*model.Asset) {
	nodes := make([]*model.NodeIdPidName, 0)
	if err := mysql.DB.
		Model(nodes).
		Find(&nodes).
		Error; err != nil {
		logger.L.Error("asset posthookfailed", zap.Error(err))
		return
	}
	g := make(map[int][]model.Pair[int, string])
	for _, n := range nodes {
		g[n.ParentId] = append(g[n.ParentId], model.Pair[int, string]{First: n.Id, Second: n.Name})
	}
	m := make(map[int]string)
	var dfs func(int, string)
	dfs = func(x int, s string) {
		m[x] = s
		for _, node := range g[x] {
			dfs(node.First, fmt.Sprintf("%s/%s", s, node.Second))
		}
	}
	dfs(0, "")

	for _, d := range data {
		d.NodeChain = m[d.ParentId]
	}
}

func assetPostHookAuth(ctx *gin.Context, data []*model.Asset) {
	currentUser, _ := acl.GetSessionFromCtx(ctx)
	if acl.IsAdmin(currentUser) {
		return
	}
	for _, a := range data {
		for k, v := range a.Authorization {
			if lo.Contains(v, currentUser.GetRid()) {
				continue
			}
			delete(a.Authorization, k)
		}
	}
}

func handleParentId(parentId int) (pids []int, err error) {
	nodes := make([]*model.NodeIdPid, 0)
	if err = mysql.DB.Model(nodes).Find(&nodes).Error; err != nil {
		return
	}
	g := make(map[int][]int)
	for _, n := range nodes {
		g[n.ParentId] = append(g[n.ParentId], n.Id)
	}
	var dfs func(int)
	dfs = func(x int) {
		pids = append(pids, x)
		for _, y := range g[x] {
			dfs(y)
		}
	}
	dfs(parentId)

	return
}
