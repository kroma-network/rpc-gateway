package mysql

import (
	"crypto/md5"
	"encoding/json"
	"strconv"
	"time"

	"github.com/Conflux-Chain/confura/util/acl"
	"github.com/Conflux-Chain/confura/util/rate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MysqlConfKeyReorgVersion = "reorg.version"

	// pre-defined ratelimit strategy config key prefix
	RateLimitStrategyConfKeyPrefix   = "ratelimit.strategy."
	rateLimitStrategySqlMatchPattern = RateLimitStrategyConfKeyPrefix + "%"

	// pre-defined access control config key prefix
	AclAllowListConfKeyPrefix   = "acl.allowlist."
	aclAllowListSqlMatchPattern = AclAllowListConfKeyPrefix + "%"

	// pre-defined node route group config key prefix
	NodeRouteGroupConfKeyPrefix   = "noderoute.group."
	nodeRouteGroupSqlMatchPattern = NodeRouteGroupConfKeyPrefix + "%"
)

// configuration tables
type conf struct {
	ID        uint32
	Name      string `gorm:"unique;size:128;not null"` // config name
	Value     string `gorm:"size:16250;not null"`      // config value
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (conf) TableName() string {
	return "configs"
}

type confStore struct {
	*baseStore
}

func newConfStore(db *gorm.DB) *confStore {
	return &confStore{
		baseStore: newBaseStore(db),
	}
}

func (cs *confStore) LoadConfig(confNames ...string) (map[string]interface{}, error) {
	var confs []conf

	if err := cs.db.Where("name IN ?", confNames).Find(&confs).Error; err != nil {
		return nil, err
	}

	res := make(map[string]interface{}, len(confs))
	for _, c := range confs {
		res[c.Name] = c.Value
	}

	return res, nil
}

func (cs *confStore) StoreConfig(confName string, confVal interface{}) error {
	return cs.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "name"}},
		DoUpdates: clause.Assignments(map[string]interface{}{"value": confVal}),
	}).Create(&conf{
		Name:  confName,
		Value: confVal.(string),
	}).Error
}

func (cs *confStore) DeleteConfig(confName string) (bool, error) {
	res := cs.db.Delete(&conf{}, "name = ?", confName)
	return res.RowsAffected > 0, res.Error
}

// reorg config

func (cs *confStore) GetReorgVersion() (int, error) {
	var result conf
	exists, err := cs.exists(&result, "name = ?", MysqlConfKeyReorgVersion)
	if err != nil {
		return 0, err
	}

	if !exists {
		return 0, nil
	}

	return strconv.Atoi(result.Value)
}

// thread unsafe
func (cs *confStore) createOrUpdateReorgVersion(dbTx *gorm.DB) error {
	version, err := cs.GetReorgVersion()
	if err != nil {
		return err
	}

	newVersion := strconv.Itoa(version + 1)

	return cs.StoreConfig(MysqlConfKeyReorgVersion, newVersion)
}

// access control config
func (cs *confStore) LoadAclAllowList(name string) (*acl.AllowList, error) {
	var cfg conf
	if err := cs.db.Where("name = ?", AclAllowListConfKeyPrefix+name).First(&cfg).Error; err != nil {
		return nil, err
	}

	return cs.decodeAclAllowLists(cfg)
}

func (cs *confStore) LoadAclAllowListById(aclID uint32) (*acl.AllowList, error) {
	cfg := conf{ID: aclID}
	if err := cs.db.First(&cfg).Error; err != nil {
		return nil, err
	}

	return cs.decodeAclAllowLists(cfg)
}

func (cs *confStore) LoadAclAllowListConfigs() (map[uint32]*acl.AllowList, map[uint32][md5.Size]byte, error) {
	var cfgs []conf
	if err := cs.db.Where("name LIKE ?", aclAllowListSqlMatchPattern).Find(&cfgs).Error; err != nil {
		return nil, nil, err
	}

	if len(cfgs) == 0 {
		return nil, nil, nil
	}

	allowLists := make(map[uint32]*acl.AllowList)
	checksums := make(map[uint32][md5.Size]byte)

	// decode allow lists from access control configs
	for _, v := range cfgs {
		al, err := cs.decodeAclAllowLists(v)
		if err != nil {
			logrus.WithField("cfg", v).WithError(err).Warn("Invalid access control allowlist config")
			continue
		}

		allowLists[v.ID] = al
		checksums[v.ID] = md5.Sum([]byte(v.Value))
	}

	return allowLists, checksums, nil
}

func (cs *confStore) decodeAclAllowLists(cfg conf) (*acl.AllowList, error) {
	// eg., acl.allowlists.fluent
	name := cfg.Name[len(AclAllowListConfKeyPrefix):]
	if len(name) == 0 {
		return nil, errors.New("allowlist name is too short")
	}

	data := []byte(cfg.Value)
	al := acl.NewAllowList(cfg.ID, name)

	if err := json.Unmarshal(data, al); err != nil {
		return nil, err
	}

	return al, nil
}

// ratelimit config

func (cs *confStore) LoadRateLimitConfigs() (*rate.Config, error) {
	rlStrategies, csStrategies, err := cs.LoadRateLimitStrategyConfigs()
	if err != nil {
		return nil, err
	}

	aclAllowLists, csAllowLists, err := cs.LoadAclAllowListConfigs()
	if err != nil {
		return nil, err
	}

	return &rate.Config{
		CheckSums: rate.ConfigCheckSums{
			Strategies: csStrategies,
			AllowLists: csAllowLists,
		},
		Strategies: rlStrategies,
		AllowLists: aclAllowLists,
	}, nil
}

func (cs *confStore) LoadRateLimitStrategy(name string) (*rate.Strategy, error) {
	var cfg conf
	if err := cs.db.Where("name = ?", RateLimitStrategyConfKeyPrefix+name).First(&cfg).Error; err != nil {
		return nil, err
	}

	return cs.decodeRateLimitStrategy(cfg)
}

func (cs *confStore) LoadRateLimitStrategyConfigs() (map[uint32]*rate.Strategy, map[uint32][md5.Size]byte, error) {
	var cfgs []conf
	if err := cs.db.Where("name LIKE ?", rateLimitStrategySqlMatchPattern).Find(&cfgs).Error; err != nil {
		return nil, nil, err
	}

	if len(cfgs) == 0 {
		return nil, nil, nil
	}

	strategies := make(map[uint32]*rate.Strategy)
	checksums := make(map[uint32][md5.Size]byte)

	// decode ratelimit strategy from config item
	for _, v := range cfgs {
		strategy, err := cs.decodeRateLimitStrategy(v)
		if err != nil {
			logrus.WithField("cfg", v).WithError(err).Warn("Invalid rate limit strategy config")
			continue
		}

		strategies[v.ID] = strategy
		checksums[v.ID] = md5.Sum([]byte(v.Value))
	}

	return strategies, checksums, nil
}

func (cs *confStore) decodeRateLimitStrategy(cfg conf) (*rate.Strategy, error) {
	// eg., ratelimit.strategy.whitelist
	name := cfg.Name[len(RateLimitStrategyConfKeyPrefix):]
	if len(name) == 0 {
		return nil, errors.New("strategy name is too short")
	}

	data := []byte(cfg.Value)
	stg := rate.NewStrategy(cfg.ID, name)

	if err := json.Unmarshal(data, stg); err != nil {
		return nil, err
	}

	return stg, nil
}

// node route config

type NodeRouteGroup struct {
	ID    uint32   `json:"-"`     // group ID
	Name  string   `json:"-"`     // group name
	Nodes []string `json:"nodes"` // node urls
}

func (cs *confStore) StoreNodeRouteGroup(routeGrp *NodeRouteGroup) error {
	cfgVal, err := json.Marshal(routeGrp)
	if err != nil {
		return errors.WithMessage(err, "failed to marshal node route group")
	}

	cfgKey := NodeRouteGroupConfKeyPrefix + routeGrp.Name
	return cs.StoreConfig(cfgKey, string(cfgVal))
}

func (cs *confStore) DelNodeRouteGroup(group string) error {
	cfgKey := NodeRouteGroupConfKeyPrefix + group
	_, err := cs.DeleteConfig(cfgKey)
	return err
}

func (cs *confStore) LoadNodeRouteGroups(inclusiveGroups ...string) (res map[string]*NodeRouteGroup, err error) {
	var nodeRouteGrpConfKeys []string
	for _, grp := range inclusiveGroups {
		confKey := NodeRouteGroupConfKeyPrefix + grp
		nodeRouteGrpConfKeys = append(nodeRouteGrpConfKeys, confKey)
	}

	var cfgs []conf

	if len(nodeRouteGrpConfKeys) == 0 {
		err = cs.db.Where("name LIKE ?", nodeRouteGroupSqlMatchPattern).Find(&cfgs).Error
	} else {
		err = cs.db.Where("name IN (?)", nodeRouteGrpConfKeys).Find(&cfgs).Error
	}

	if err != nil {
		return nil, err
	}

	if len(cfgs) == 0 { // no data
		return nil, nil
	}

	res = make(map[string]*NodeRouteGroup)

	// decode node route group from config item
	for _, v := range cfgs {
		grp, err := cs.decodeNodeRouteGroup(v)
		if err != nil {
			logrus.WithField("cfg", v).WithError(err).Warn("Invalid node route config")
			continue
		}

		res[grp.Name] = grp
	}

	return res, nil
}

func (cs *confStore) decodeNodeRouteGroup(cfg conf) (*NodeRouteGroup, error) {
	// eg., noderoute.group.cfxvip
	name := cfg.Name[len(NodeRouteGroupConfKeyPrefix):]
	if len(name) == 0 {
		return nil, errors.New("route group name is too short")
	}

	grp := NodeRouteGroup{ID: cfg.ID, Name: name}
	data := []byte(cfg.Value)

	if err := json.Unmarshal(data, &grp); err != nil {
		return nil, err
	}

	return &grp, nil
}
