package recommend

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/auxten/edgeRec/feature/embedding"
	"github.com/auxten/edgeRec/feature/embedding/model"
	"github.com/auxten/edgeRec/feature/embedding/model/word2vec"
	"github.com/auxten/edgeRec/ps"
	"github.com/auxten/edgeRec/utils"
	"github.com/karlseguin/ccache/v2"
	log "github.com/sirupsen/logrus"
	"gonum.org/v1/gonum/mat"
)

const (
	SampleAssembler       = 16
	StageKey              = "stage"
	ItemEmbDim            = 16
	ItemEmbWindow         = 5
	UserBehaviorLen       = 10
	userFeatureCacheSize  = 200000
	itemFeatureCacheSize  = 2000000
	userBehaviorCacheSize = userFeatureCacheSize * UserBehaviorLen
)

var (
	itemEmbeddingModel model.Model
	itemEmbeddingMap   word2vec.EmbeddingMap
	//TODO: maybe a switch to control whether to reuse training cache when predict
	UserFeatureCache  *ccache.Cache
	ItemFeatureCache  *ccache.Cache
	UserBehaviorCache *ccache.Cache

	// DefaultUserFeature and DefaultItemFeature are backup if not nil
	//when user or item missing in database, use this to fill
	DefaultUserFeature []float64
	DefaultItemFeature []float64

	DebugUserId int
	DebugItemId int
)

type Tensor []float64

type Stage int

const (
	TrainStage Stage = iota
	PredictStage
)

type TrainSample struct {
	Data ps.Samples
	Info SampleInfo
}

type sampleVec struct {
	vec    []float64
	label  float64
	iWidth int
	uWidth int
}

type RecSys interface {
	BasicFeatureProvider
	Trainer
}

type Predictor interface {
	BasicFeatureProvider
	PredictAbstract
}

type BasicFeatureProvider interface {
	UserFeaturer
	ItemFeaturer
}

type PredictAbstract interface {
	Predict(X mat.Matrix, Y mat.Mutable) *mat.Dense
}

type Trainer interface {
	SampleGenerator(context.Context) (<-chan Sample, error)
}

type Fitter interface {
	Fit(sample *TrainSample) (PredictAbstract, error)
}

type ItemFeaturer interface {
	GetItemFeature(context.Context, int) (Tensor, error)
}

type UserFeaturer interface {
	GetUserFeature(context.Context, int) (Tensor, error)
}

// UserBehavior interface is used to get user behavior feature.
// typically, it is user's clicked/bought/liked item id list ordered by time desc.
// During training, you should limit the seq to avoid time travel,
//
//		maxPk or maxTs could be used here:
//		 - maxPk is the max primary key of user behavior table.
//		 - maxTs is the max timestamp of user behavior table.
//		 - maxLen is the max length of user behavior seq, if total len is
//			greater than maxLen, the seq will be truncated from the tail.
//	 	which is latest maxLen items.
//
// specially, -1 means no limit.
// During prediction, you should use the latest user behavior seq.
type UserBehavior interface {
	GetUserBehavior(ctx context.Context, userId int,
		maxLen int64, maxPk int64, maxTs int64) (itemSeq []int, err error)
}

// ItemEmbedding is an interface used to generate item embedding with item2vec model
// by just providing a behavior based item sequence.
// Example: user liked items sequence, user bought items sequence, user viewed items sequence
type ItemEmbedding interface {
	ItemSeqGenerator(context.Context) (<-chan string, error)
}

type SampleInfo struct {
	UserProfileRange  [2]int // [start, end)
	UserBehaviorRange [2]int // [start, end)
	ItemFeatureRange  [2]int // [start, end)
	CtxFeatureRange   [2]int // [start, end)
}

type UserItemOverview struct {
	UserId       int `json:"user_id"`
	UserFeatures map[string]interface{}
}

type ItemOverView struct {
	ItemId       int `json:"item_id"`
	ItemFeatures map[string]interface{}
}

type UserItemOverviewResult struct {
	Users []UserItemOverview `json:"users"`
}

type ItemOverviewResult struct {
	Items []ItemOverView `json:"items"`
}

type DashboardOverviewResult struct {
	Users         int `json:"users"`
	Items         int `json:"items"`
	TotalPositive int `json:"total_positive"`
	ValidPositive int `json:"valid_positive"`
	ValidNegative int `json:"valid_negative"`
}

type FeatureOverview interface {
	// GetUsersFeatureOverview returns offset and size used for paging query
	GetUsersFeatureOverview(ctx context.Context, offset, size int, opts map[string][]string) (UserItemOverviewResult, error)

	// GetItemsFeatureOverview returns offset and size used for paging query
	GetItemsFeatureOverview(ctx context.Context, offset, size int, opts map[string][]string) (ItemOverviewResult, error)

	// GetDashboardOverview returns dashboard overview, see DashboardOverviewResult
	GetDashboardOverview(ctx context.Context) (DashboardOverviewResult, error)
}

type PreRanker interface {
	PreRank(context.Context) error
}

type PreTrainer interface {
	PreTrain(context.Context) error
}

type ItemScore struct {
	ItemId int     `json:"itemId"`
	Score  float64 `json:"score"`
}

type Sample struct {
	UserId    int     `json:"userId"`
	ItemId    int     `json:"itemId"`
	Label     float64 `json:"label"`
	Timestamp int64   `json:"timestamp"`
}

func Train(ctx context.Context, recSys RecSys, mlp Fitter) (model Predictor, err error) {
	ctx = context.WithValue(ctx, StageKey, TrainStage)

	if preTrain, ok := recSys.(PreTrainer); ok {
		err = preTrain.PreTrain(ctx)
		if err != nil {
			log.Errorf("pre train error: %v", err)
			return
		}
	}

	if itemEbd, ok := recSys.(ItemEmbedding); ok {
		itemEmbeddingModel, err = GetItemEmbeddingModelFromUb(ctx, itemEbd)
		if err != nil {
			log.Errorf("get item embedding model error: %v", err)
			return
		}
		itemEmbeddingMap, err = itemEmbeddingModel.GenEmbeddingMap()
		if err != nil {
			log.Errorf("get item embedding map error: %v", err)
			return
		}
	}

	trainSample, err := GetSample(recSys, ctx)
	sampleLen := len(trainSample.Data)

	// start training
	log.Infof("\nstart training with %d samples\n", sampleLen)

	pred, err := mlp.Fit(trainSample)
	if err != nil {
		log.Errorf("fit error: %v", err)
		return
	}
	type modelImpl struct {
		UserFeaturer
		ItemFeaturer
		PredictAbstract
	}
	model = &modelImpl{
		UserFeaturer:    recSys,
		ItemFeaturer:    recSys,
		PredictAbstract: pred,
	}

	return
}

func Rank(ctx context.Context, recSys Predictor, userId int, itemIds []int) (itemScores []ItemScore, err error) {
	sampleKeys := make([]Sample, len(itemIds))
	for i, itemId := range itemIds {
		sampleKeys[i] = Sample{
			UserId:    userId,
			ItemId:    itemId,
			Timestamp: time.Now().Unix(),
		}
	}
	y, err := BatchPredict(ctx, recSys, sampleKeys)
	if err != nil {
		return
	}
	itemScores = make([]ItemScore, len(itemIds))
	for i, itemId := range itemIds {
		itemScores[i] = ItemScore{
			ItemId: itemId,
			Score:  y.At(i, 0),
		}
	}

	return
}

func BatchPredict(ctx context.Context, recSys Predictor, sampleKeys []Sample) (y *mat.Dense, err error) {
	ctx = context.WithValue(ctx, StageKey, PredictStage)
	if preRanker, ok := recSys.(PreRanker); ok {
		err = preRanker.PreRank(ctx)
		if err != nil {
			log.Errorf("pre rank error: %v", err)
			return
		}
	}

	y = mat.NewDense(len(sampleKeys), 1, nil)
	var (
		x          *mat.Dense
		zeroSliceX []float64
		debugIds   = make([]int, 0)
	)

	for i, sKey := range sampleKeys {
		var (
			xSlice []float64
		)
		xSlice, _, _, err = GetSampleVector(ctx, UserFeatureCache, ItemFeatureCache, recSys, &sKey)
		if err != nil {
			if i == 0 {
				log.Errorf("get sample vector error: %v", err)
				return
			} else {
				_, col := x.Dims()
				zeroSliceX = make([]float64, col)
				xSlice = zeroSliceX
			}
		}
		if i == 0 {
			x = mat.NewDense(len(sampleKeys), len(xSlice), nil)
		}

		_, xCol := x.Dims()
		if len(xSlice) != xCol {
			log.Errorf("x slice length %d != x col %d", len(xSlice), xCol)
			return
		}
		x.SetRow(i, xSlice)
		if DebugItemId == sKey.ItemId &&
			(DebugUserId == 0 || DebugUserId == sKey.UserId) {
			log.Infof("user %d: item %d: feature %v", sKey.UserId, sKey.ItemId, xSlice)
			debugIds = append(debugIds, i)
		}
	}
	recSys.Predict(x, y)
	for _, i := range debugIds {
		log.Infof("user %d: item %d: score %v", sampleKeys[i].UserId, sampleKeys[i].ItemId, y.At(i, 0))
	}
	return
}

func GetSample(recSys RecSys, ctx context.Context) (sample *TrainSample, err error) {
	var (
		sampleWidth      int
		userFeatureWidth int
		itemFeatureWidth int
	)
	if UserFeatureCache == nil {
		UserFeatureCache = ccache.New(
			ccache.Configure().MaxSize(userFeatureCacheSize).ItemsToPrune(userFeatureCacheSize / 100),
		)
	}
	if ItemFeatureCache == nil {
		ItemFeatureCache = ccache.New(
			ccache.Configure().MaxSize(itemFeatureCacheSize).ItemsToPrune(itemFeatureCacheSize / 100),
		)
	}

	//defer func() {
	//	UserFeatureCache.Clear()
	//	ItemFeatureCache.Clear()
	//	UserBehaviorCache.Clear()
	//}()

	sampleGen, ok := recSys.(Trainer)
	if !ok {
		panic("sample generator not implemented")
	}
	sampleCh, err := sampleGen.SampleGenerator(ctx)
	if err != nil {
		panic(err)
	}

	var (
		sampleVecCh = make(chan *sampleVec, 1000)
		sampleVecWg sync.WaitGroup
	)

	for c := 0; c < SampleAssembler; c++ {
		sampleVecWg.Add(1)
		go func() {
			for s := range sampleCh {
				var (
					err  error
					sVec sampleVec
				)
				sVec.vec, sVec.uWidth, sVec.iWidth, err = GetSampleVector(ctx, UserFeatureCache, ItemFeatureCache, recSys, &s)
				if err != nil {
					log.Debugf("get sample vector error: %v", err)
					continue
				}
				sVec.label = s.Label
				sampleVecCh <- &sVec
			}
			sampleVecWg.Done()
		}()
	}
	go func() {
		sampleVecWg.Wait()
		close(sampleVecCh)
	}()

	sample = &TrainSample{}
	for sv := range sampleVecCh {
		if userFeatureWidth == 0 {
			userFeatureWidth = sv.uWidth
			sample.Info.UserProfileRange[0] = 0
			sample.Info.UserProfileRange[1] = userFeatureWidth
			sample.Info.UserBehaviorRange[0] = sample.Info.UserProfileRange[1]
			sample.Info.UserBehaviorRange[1] = sample.Info.UserProfileRange[1] + ItemEmbDim*UserBehaviorLen
			// item feature here is only embeddings
			sample.Info.ItemFeatureRange[0] = sample.Info.UserBehaviorRange[1]
			sample.Info.ItemFeatureRange[1] = sample.Info.UserBehaviorRange[1] + ItemEmbDim
		}
		if sv.uWidth != userFeatureWidth {
			err = fmt.Errorf("user feature length mismatch: %v:%v",
				userFeatureWidth, sv.uWidth)
			return
		}

		if itemFeatureWidth == 0 {
			itemFeatureWidth = sv.iWidth
			// non embedding item feature is treated as ctx feature
			sample.Info.CtxFeatureRange[0] = sample.Info.ItemFeatureRange[1]
			sample.Info.CtxFeatureRange[1] = sample.Info.ItemFeatureRange[1] + itemFeatureWidth
		}
		if sv.iWidth != itemFeatureWidth {
			err = fmt.Errorf("item feature length mismatch: %v:%v",
				itemFeatureWidth, sv.iWidth)
			return
		}

		if sampleWidth == 0 {
			sampleWidth = len(sv.vec)
		} else {
			if len(sv.vec) != sampleWidth {
				err = fmt.Errorf("sample width mismatch: %v:%v", sampleWidth, len(sv.vec))
				return
			}
		}

		sample.Data = append(sample.Data, ps.Sample{
			Input:    sv.vec,
			Response: []float64{sv.label},
		})
		if len(sample.Data)%1000 == 0 {
			log.Infof("sample size: %d", len(sample.Data))
		}
	}

	return
}

func GetSampleVector(ctx context.Context,
	userFeatureCache *ccache.Cache, itemFeatureCache *ccache.Cache,
	featureProvider BasicFeatureProvider, sampleKey *Sample,
) (vec []float64, userFeatureWidth int, itemFeatureWidth int, err error) {
	var (
		zeroItemEmb       [ItemEmbDim]float64
		zeroUserBehaviors [ItemEmbDim * UserBehaviorLen]float64

		user, item *ccache.Item
	)
	userIdStr := strconv.Itoa(sampleKey.UserId)
	user, err = userFeatureCache.Fetch(userIdStr, time.Hour*24, func() (ci interface{}, err error) {
		ci, err = featureProvider.GetUserFeature(ctx, sampleKey.UserId)
		return
	})
	if err != nil {
		return
	}
	userFeature := user.Value().(Tensor)
	userFeatureWidth = len(userFeature)

	itemIdStr := strconv.Itoa(sampleKey.ItemId)
	item, err = itemFeatureCache.Fetch(itemIdStr, time.Hour*24, func() (ci interface{}, err error) {
		ci, err = featureProvider.GetItemFeature(ctx, sampleKey.ItemId)
		return
	})
	if err != nil {
		return
	}
	itemFeature := item.Value().(Tensor)
	itemFeatureWidth = len(itemFeature)

	// if ItemEmbedding interface is implemented, use item embedding,
	// 	else use zero embedding.
	var (
		itemEmb       = zeroItemEmb[:]
		userBehaviors = zeroUserBehaviors[:]
		ok            bool
	)
	if len(itemEmbeddingMap) != 0 {
		if itemEmb, ok = itemEmbeddingMap.Get(strconv.Itoa(sampleKey.ItemId)); !ok {
			itemEmb = zeroItemEmb[:]
			log.Debugf("item embedding not found: %d, using zeros", sampleKey.ItemId)
		}
		// if ItemEmbedding and UserBehavior interface are both implemented,
		// use itemSeq embeddings got from GetUserBehavior as user behavior,
		//	else use zero embedding.
		if recSysUb, ok := featureProvider.(UserBehavior); ok {
			getUbfunc := func(userId int, maxLen int64, maxPk int64, maxTs int64) (ubTensor Tensor, err error) {
				itemSeq, err := recSysUb.GetUserBehavior(
					ctx, userId, maxLen, maxPk, maxTs)
				if err != nil {
					return
				}
				//query items embedding, fill them into user behavior
				ubTensor = make(Tensor, ItemEmbDim*UserBehaviorLen)
				for i, itemId := range itemSeq {
					if itemEmb, ok := itemEmbeddingMap.Get(strconv.Itoa(itemId)); ok {
						copy(ubTensor[i*ItemEmbDim:], itemEmb)
					}
				}
				return
			}
			userBehaviors, err = getUbfunc(sampleKey.UserId, UserBehaviorLen, -1, sampleKey.Timestamp)
			if err != nil {
				err = fmt.Errorf("get user behavior error: %v", err)
				return
			}
		}
	}

	vec = utils.ConcatSlice(userFeature, userBehaviors, itemEmb, itemFeature)

	return
}

func GetItemEmbeddingModelFromUb(ctx context.Context, iSeq ItemEmbedding) (mod model.Model, err error) {
	itemSeq, err := iSeq.ItemSeqGenerator(ctx)
	if err != nil {
		return
	}
	mod, err = embedding.TrainEmbedding(itemSeq, ItemEmbWindow, ItemEmbDim, 1)
	return
}
