// Copyright 2014 The roc Author. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rocserv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.pri.ibanyu.com/middleware/seaweed/xconfig"
	"gitlab.pri.ibanyu.com/middleware/seaweed/xconfig/apollo"
	"gitlab.pri.ibanyu.com/middleware/seaweed/xcontext"
	"gitlab.pri.ibanyu.com/middleware/seaweed/xlog"
	xmgo "gitlab.pri.ibanyu.com/middleware/seaweed/xmgo/manager"
	xsql "gitlab.pri.ibanyu.com/middleware/seaweed/xsql/manager"
	xprom "gitlab.pri.ibanyu.com/middleware/seaweed/xstat/xmetric/xprometheus"
	"gitlab.pri.ibanyu.com/middleware/seaweed/xtransport/gen-go/util/thriftutil"
	. "gitlab.pri.ibanyu.com/server/servmonitor/pub.git/idl/gen-go/base/servmonitorservice"

	etcd "github.com/coreos/etcd/client"
	"github.com/shawnfeng/sutil/dbrouter"
	"github.com/shawnfeng/sutil/slowid"
	"github.com/shawnfeng/sutil/snetutil"
	"github.com/shawnfeng/sutil/ssync"
)

const (
	BASE_LOC_DIST = "dist"
	// 调整了服务注册结构，为兼容老版本，BASE_LOC_DIST下也要完成之前方式的注册
	// dist2 2为版本2
	BASE_LOC_DIST_V2        = "dist2"
	BASE_LOC_BREAKER        = "breaker"
	BASE_LOC_BREAKER_GLOBAL = "breaker/global"
	BASE_LOC_ETC            = "etc"
	BASE_LOC_ETC_GLOBAL     = "etc/global"
	BASE_LOC_SKEY           = "skey"
	BASE_LOC_OP             = "op"
	BASE_LOC_DB             = "db/route"
	// 服务内分布式锁，只在单个服务副本之间使用
	BASE_LOC_LOCAL_DIST_LOCK = "lock/local"
	// 全局分布式锁，跨服务使用
	BASE_LOC_GLOBAL_DIST_LOCK = "lock/global"

	// thrift 服务注册的位置
	BASE_LOC_REG_SERV = "serve"

	// 后门注册的位置
	BASE_LOC_REG_BACKDOOR = "backdoor"

	// 服务手动配置位置
	BASE_LOC_REG_MANUAL = "manual"
	// sla metrics注册的位置
	BASE_LOC_REG_METRICS = "metrics"

	PROCESSOR_GRPC_PROPERTY_NAME = "proc_grpc"

	PROCESSOR_THRIFT_PROPERTY_NAME = "proc_thrift"

	//预演环境分组标识
	ENV_GROUP_PRE = "pre"

	// RPCConfNamespace RPC Apollo Conf Namespace
	RPCConfNamespace     = "rpc.client"
	ApplicationNamespace = "application"

	serverStatusStop = 1
)

type configEtcd struct {
	etcdAddrs  []string
	useBaseloc string
}

type ServBaseV2 struct {
	IdGenerator

	confEtcd     configEtcd
	configCenter xconfig.ConfigCenter

	dbLocation   string
	servLocation string
	servGroup    string
	servName     string
	servIp       string
	copyName     string
	sessKey      string
	startType    string

	envGroup string

	etcdClient etcd.KeysAPI

	// 跨机房服务注册
	crossRegisterClients   map[string]etcd.KeysAPI
	crossRegisterRegionIds []int

	servId int

	dbRouter *dbrouter.Router

	muLocks ssync.Mutex
	locks   map[string]*ssync.Mutex

	muHearts ssync.Mutex
	hearts   map[string]*distLockHeart

	stop       int32
	onShutdown func()
	reporter   Reporter
	doReport   func(context.Context, *ReportLogReq) error

	muReg    sync.Mutex
	regInfos map[string]string
}

func (m *ServBaseV2) isStop() bool {
	stopStatus := atomic.LoadInt32(&m.stop)
	return stopStatus == serverStatusStop
}

// Stop server stop
func (m *ServBaseV2) Stop() {
	m.setStatusToStop()
	m.clearRegisterInfos()
	m.clearCrossDCRegisterInfos()
	m.onShutdown()
}

// SetOnShutdown add shutdown hook in app
func (m *ServBaseV2) SetOnShutdown(onShutdown func()) {
	m.onShutdown = onShutdown
}

func (m *ServBaseV2) setStatusToStop() {
	atomic.StoreInt32(&m.stop, serverStatusStop)
}

func (m *ServBaseV2) addRegisterInfo(path, regInfo string) {
	m.muReg.Lock()
	defer m.muReg.Unlock()

	m.regInfos[path] = regInfo
}

func (m *ServBaseV2) clearRegisterInfos() {
	fun := "ServBaseV2.clearRegisterInfos -->"

	//延迟清理注册信息,防止新实例还没有完成注册
	time.Sleep(time.Second * 2)

	m.muReg.Lock()
	defer m.muReg.Unlock()

	for path, _ := range m.regInfos {
		_, err := m.etcdClient.Delete(context.Background(), path, &etcd.DeleteOptions{
			Recursive: true,
		})
		if err != nil {
			xlog.Warnf(context.Background(), "%s path: %s, err: %v", fun, path, err)
		}
	}
}

func (m *ServBaseV2) RegisterBackDoor(servs map[string]*ServInfo) error {
	rd := NewRegData(servs, m.envGroup)
	js, err := json.Marshal(rd)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s/%s/%d/%s", m.confEtcd.useBaseloc, BASE_LOC_DIST_V2, m.servLocation, m.servId, BASE_LOC_REG_BACKDOOR)

	return m.doRegister(path, string(js), true)
}

func (m *ServBaseV2) RegisterMetrics(servs map[string]*ServInfo) error {
	rd := NewRegData(servs, m.envGroup)
	js, err := json.Marshal(rd)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s/%s/%d/%s", m.confEtcd.useBaseloc, BASE_LOC_DIST_V2, m.servLocation, m.servId, BASE_LOC_REG_METRICS)

	return m.doRegister(path, string(js), true)
}

func (m *ServBaseV2) setIp() error {
	addr, err := snetutil.GetListenAddr("")
	if err != nil {
		return err
	}
	fields := strings.Split(addr, ":")
	if len(fields) < 1 {
		return fmt.Errorf("get listen addr error")
	}
	m.servIp = fields[0]
	return nil
}

// {type:http/thrift, addr:10.3.3.3:23233, processor:fuck}
func (m *ServBaseV2) RegisterService(servs map[string]*ServInfo) error {
	fun := "ServBaseV2.RegisterService -->"
	ctx := context.Background()

	err := m.RegisterServiceV2(servs, BASE_LOC_REG_SERV, false)
	if err != nil {
		xlog.Errorf(ctx, "%s register server v2 failed, err: %v", fun, err)
		return err
	}

	err = m.RegisterServiceV1(servs, false)
	if err != nil {
		xlog.Errorf(ctx, "%s register server v1 failed, err: %v", fun, err)
		return err
	}

	xlog.Infof(ctx, "%s register server ok", fun)

	return nil
}

func (m *ServBaseV2) RegisterServiceV2(servs map[string]*ServInfo, dir string, crossDC bool) error {
	rd := NewRegData(servs, m.envGroup)
	js, err := json.Marshal(rd)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/%s/%s/%d/%s", m.confEtcd.useBaseloc, BASE_LOC_DIST_V2, m.servLocation, m.servId, dir)

	// 非跨机房
	if !crossDC {
		return m.doRegister(path, string(js), true)
	}
	// 跨机房
	return m.doCrossDCRegister(path, string(js), true)
}

// 为兼容老的client发现服务，保留的
func (m *ServBaseV2) RegisterServiceV1(servs map[string]*ServInfo, crossDC bool) error {
	fun := "ServBaseV2.RegisterServiceV1 -->"

	js, err := json.Marshal(servs)
	if err != nil {
		return err
	}

	xlog.Infof(context.Background(), "%s servs:%s", fun, js)

	path := fmt.Sprintf("%s/%s/%s/%d", m.confEtcd.useBaseloc, BASE_LOC_DIST, m.servLocation, m.servId)

	// 非跨机房
	if !crossDC {
		return m.doRegister(path, string(js), true)
	}
	// 跨机房
	return m.doCrossDCRegister(path, string(js), true)
}

func (m *ServBaseV2) SetGroupAndDisable(group string, disable bool) error {
	fun := "ServBaseV2.SetGroupAndDisable -->"
	ctx := context.Background()

	path := fmt.Sprintf("%s/%s/%s/%d/%s", m.confEtcd.useBaseloc, BASE_LOC_DIST_V2, m.servLocation, m.servId, BASE_LOC_REG_MANUAL)
	value, err := m.getValueFromEtcd(path)
	if err != nil {
		xlog.Warnf(ctx, "%s getValueFromEtcd err, path:%s, err:%v", fun, path, err)
	}

	manual := &ManualData{}
	err = json.Unmarshal([]byte(value), manual)
	if len(value) > 0 && err != nil {
		xlog.Errorf(ctx, "%s unmarshal err, value:%s, err:%v", fun, value, err)
		return err
	}

	if manual.Ctrl == nil {
		manual.Ctrl = &ServCtrl{}
	}

	isFind := false
	for _, g := range manual.Ctrl.Groups {
		if g == group {
			isFind = true
			break
		}
	}

	if isFind == false {
		manual.Ctrl.Groups = append(manual.Ctrl.Groups, group)
	}
	if manual.Ctrl.Weight == 0 {
		manual.Ctrl.Weight = 100
	}
	manual.Ctrl.Disable = disable

	newValue, err := json.Marshal(manual)
	if err != nil {
		xlog.Errorf(ctx, "%s marshal err, manual:%v, err:%v", fun, manual, err)
		return err
	}

	xlog.Infof(ctx, "%s path:%s old value:%s new value:%s", fun, path, value, newValue)
	err = m.setValueToEtcd(path, string(newValue), nil)
	if err != nil {
		xlog.Errorf(ctx, "%s setValueToEtcd err, path:%s value:%s", fun, path, newValue)
	}

	return err
}

func (m *ServBaseV2) getValueFromEtcd(path string) (value string, err error) {
	fun := "ServBaseV2.getValueFromEtcd -->"
	ctx := context.Background()

	r, err := m.etcdClient.Get(context.Background(), path, &etcd.GetOptions{Recursive: false, Sort: false})
	if err != nil {
		xlog.Warnf(ctx, "%s path:%s err:%v", fun, path, err)
		return "", err
	}
	if r != nil && r.Node != nil {
		return r.Node.Value, nil
	}

	return "", nil
}

func (m *ServBaseV2) setValueToEtcd(path, value string, opts *etcd.SetOptions) error {
	fun := "ServBaseV2.setValueToEtcd -->"

	_, err := m.etcdClient.Set(context.Background(), path, value, opts)
	if err != nil {
		xlog.Errorf(context.Background(), "%s path:%s value:%s opts:%v", fun, path, value, opts)
	}

	return err
}

func (m *ServBaseV2) doRegister(path, js string, refresh bool) error {
	fun := "ServBaseV2.doRegister -->"
	ctx := context.Background()
	m.addRegisterInfo(path, js)

	// 创建完成标志
	var isCreated bool

	go func() {
		for i := 0; ; i++ {
			updateEtcd := func() {
				var err error
				if !isCreated {
					xlog.Warnf(ctx, "%s create node, round: %d server_info: %s", fun, i, js)
					_, err = m.etcdClient.Set(context.Background(), path, js, &etcd.SetOptions{
						TTL: time.Second * 60,
					})
				} else {
					if refresh {
						// 在刷新ttl时候，不允许变更value
						_, err = m.etcdClient.Set(context.Background(), path, "", &etcd.SetOptions{
							PrevExist: etcd.PrevExist,
							TTL:       time.Second * 60,
							Refresh:   true,
						})
					} else {
						_, err = m.etcdClient.Set(context.Background(), path, js, &etcd.SetOptions{
							TTL: time.Second * 60,
						})
					}

				}

				if err != nil {
					isCreated = false
					xlog.Warnf(ctx, "%s register need create node, round: %d, err: %v", fun, i, err)

				} else {
					isCreated = true
				}
			}

			withRegLockRunClosureBeforeStop(m, ctx, fun, updateEtcd)

			time.Sleep(time.Second * 20)

			if m.isStop() {
				xlog.Infof(ctx, "%s server stop, register path [%s] clear", fun, path)
				return
			}
		}

	}()

	return nil
}

func (m *ServBaseV2) Servid() int {
	return m.servId
}

func (m *ServBaseV2) Copyname() string {
	return fmt.Sprintf("%s%d", m.servLocation, m.servId)

}

func (m *ServBaseV2) Servname() string {
	return m.servLocation
}

func (m *ServBaseV2) ServIp() string {
	return m.servIp
}

func (m *ServBaseV2) Dbrouter() *dbrouter.Router {
	return m.dbRouter
}

// ConfigCenter ...
func (m *ServBaseV2) ConfigCenter() xconfig.ConfigCenter {
	return m.configCenter
}

// RegInfos ...
func (m *ServBaseV2) RegInfos() map[string]string {
	m.muReg.Lock()
	defer m.muReg.Unlock()

	result := make(map[string]string, len(m.regInfos))
	for k, v := range m.regInfos {
		result[k] = v
	}
	return result
}

func (m *ServBaseV2) ServConfig(cfg interface{}) error {
	fun := "ServBaseV2.ServConfig -->"
	ctx := context.Background()
	// 获取全局配置
	path := fmt.Sprintf("%s/%s", m.confEtcd.useBaseloc, BASE_LOC_ETC_GLOBAL)
	scfg_global, err := getValue(m.etcdClient, path)
	if err != nil {
		xlog.Warnf(ctx, "%s serv config global value path: %s err: %v", fun, path, err)
	}
	xlog.Infof(ctx, "%s global cfg:%s path:%s", fun, scfg_global, path)

	path = fmt.Sprintf("%s/%s/%s", m.confEtcd.useBaseloc, BASE_LOC_ETC, m.servLocation)
	scfg, err := getValue(m.etcdClient, path)
	if err != nil {
		xlog.Warnf(context.Background(), "%s serv config value path: %s err: %v", fun, path, err)
	}

	tf := xconfig.NewTierConf()
	err = tf.Load(scfg_global)
	if err != nil {
		return err
	}

	err = tf.Load(scfg)
	if err != nil {
		return err
	}

	err = tf.Unmarshal(cfg)
	if err != nil {
		return err
	}

	return nil
}

// etcd v2 接口
func NewServBaseV2(confEtcd configEtcd, servLocation, skey, envGroup string, sidOffset int, crossRegionIdList []int) (*ServBaseV2, error) {
	fun := "NewServBaseV2 -->"
	ctx := context.Background()

	cfg := etcd.Config{
		Endpoints: confEtcd.etcdAddrs,
		Transport: etcd.DefaultTransport,
	}

	xlog.Infof(ctx, "%s create etcd client start, cfg: %v", fun, cfg)
	c, err := etcd.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create etchd client cfg error")
	}

	client := etcd.NewKeysAPI(c)
	if client == nil {
		return nil, fmt.Errorf("create etchd api error")
	}

	path := fmt.Sprintf("%s/%s/%s", confEtcd.useBaseloc, BASE_LOC_SKEY, servLocation)

	xlog.Infof(ctx, "%s retryGenSid start", fun)
	sid, err := retryGenSid(client, path, skey, 3)
	if err != nil {
		return nil, err
	}

	xlog.Infof(ctx, "%s retryGenSid end, path: %s, sid: %d, skey: %s, envGroup: %s", fun, path, sid, skey, envGroup)

	dbloc := fmt.Sprintf("%s/%s", confEtcd.useBaseloc, BASE_LOC_DB)

	xlog.Infof(ctx, "%s init dbrouter start", fun)
	var dr *dbrouter.Router
	jscfg, err := getValue(client, dbloc)
	if err != nil {
		xlog.Warnf(ctx, "%s db:%s config notfound", fun, dbloc)
	} else {
		dr, err = dbrouter.NewRouter(jscfg)
		if err != nil {
			return nil, err
		}
	}
	xlog.Infof(ctx, "%s init dbrouter end", fun)

	// init global config center
	xlog.Infof(ctx, " %s init configcenter start", fun)
	configCenter, err := xconfig.NewConfigCenter(context.TODO(), apollo.ConfigTypeApollo, servLocation, []string{ApplicationNamespace, RPCConfNamespace, xsql.MysqlConfNamespace, xmgo.MongoConfNamespace})
	if err != nil {
		return nil, err
	}
	xlog.Infof(ctx, " %s init configcenter end", fun)

	reg := &ServBaseV2{
		confEtcd:               confEtcd,
		dbLocation:             dbloc,
		servLocation:           servLocation,
		sessKey:                skey,
		etcdClient:             client,
		crossRegisterRegionIds: crossRegionIdList,
		crossRegisterClients:   make(map[string]etcd.KeysAPI, 2),
		servId:                 sid,
		locks:                  make(map[string]*ssync.Mutex),
		hearts:                 make(map[string]*distLockHeart),
		regInfos:               make(map[string]string),

		dbRouter:     dr,
		configCenter: configCenter,

		envGroup:   envGroup,
		onShutdown: func() { xlog.Info(context.TODO(), "app shutdown") },
	}

	svrInfo := strings.SplitN(servLocation, "/", 2)
	if len(svrInfo) == 2 {
		reg.servGroup = svrInfo[0]
		reg.servName = svrInfo[1]
	} else {
		xlog.Warnf(ctx, "%s servLocation:%s do not match group/service format", fun, servLocation)
	}

	xlog.Infof(ctx, " %s init origin snowflake start", fun)
	sf, err := initSnowflake(sid + sidOffset)
	if err != nil {
		return nil, err
	}
	xlog.Infof(ctx, " %s init origin snowflake end", fun)

	reg.IdGenerator.snow = sf
	reg.IdGenerator.slow = make(map[string]*slowid.Slowid)
	reg.IdGenerator.workerID = sid + sidOffset

	// init cross register clients
	xlog.Infof(ctx, " %s init CrossRegisterCenter start", fun)
	err = initCrossRegisterCenter(reg)
	if err != nil {
		return nil, err
	}
	xlog.Infof(ctx, " %s init CrossRegisterCenter end", fun)

	return reg, nil

}

func (m *ServBaseV2) isPreEnvGroup() bool {
	if m.envGroup == ENV_GROUP_PRE {
		return true
	}

	return false
}

func (m *ServBaseV2) WithControlLaneInfo(ctx context.Context) context.Context {
	value := ctx.Value(xcontext.ContextKeyControl)
	if value == nil {
		control := m.createControlWithLaneInfo()
		return context.WithValue(ctx, xcontext.ContextKeyControl, control)
	}

	v := value.(xcontext.ContextControlRouter)
	if _, ok := v.GetControlRouteGroup(); !ok {
		v.SetControlRouteGroup(m.envGroup)
	}

	return ctx
}

func (m *ServBaseV2) InitReportLog(reporter Reporter) {
	m.reporter = reporter
	if m.startType != "local" {
		go func() {
			for {
				go m.doReportLog(context.Background())
				time.Sleep(time.Second * 60)
			}
		}()
	}
}

func (m *ServBaseV2) createControlWithLaneInfo() *thriftutil.Control {
	route := thriftutil.NewRoute()
	route.Group = m.envGroup
	control := thriftutil.NewControl()
	control.Route = route
	return control
}

// mutex

func withRegLockRunClosureBeforeStop(m *ServBaseV2, ctx context.Context, funcName string, f func()) {
	startTime := time.Now()
	m.muReg.Lock()
	xlog.Infof(ctx, "%s lock muReg for update", funcName)
	defer func() {
		m.muReg.Unlock()
		duration := time.Since(startTime)
		xlog.Infof(ctx, "%s unlock muReq for update, duration: %v", funcName, duration)
	}()

	if m.isStop() {
		xlog.Infof(ctx, "%s server stop, do not run function", funcName)
		return
	}

	f()
}

func (m *ServBaseV2) SetStartType(startType string) {
	m.startType = startType
}

func (m *ServBaseV2) doReportLog(ctx context.Context) {
	// 汇报log统计
	st, logs := xlog.LogStat()
	group, serviceName := GetGroupAndService()
	_metricLogCount := GetLogCountMetric()
	_metricLogCount.With(xprom.LabelGroupName, group, xprom.LabelServiceName, serviceName, xprom.LabelType, "LOG_DEBUGCN").Add(float64(st["DEBUG"]))
	_metricLogCount.With(xprom.LabelGroupName, group, xprom.LabelServiceName, serviceName, xprom.LabelType, "LOG_TRACECN").Add(float64(st["TRACE"]))
	_metricLogCount.With(xprom.LabelGroupName, group, xprom.LabelServiceName, serviceName, xprom.LabelType, "LOG_INFOCN").Add(float64(st["INFO"]))
	_metricLogCount.With(xprom.LabelGroupName, group, xprom.LabelServiceName, serviceName, xprom.LabelType, "LOG_WARNCN").Add(float64(st["WARN"]))
	_metricLogCount.With(xprom.LabelGroupName, group, xprom.LabelServiceName, serviceName, xprom.LabelType, "LOG_ERRORCN").Add(float64(st["ERROR"]))
	servname := fmt.Sprintf("%s/%s", group, serviceName)
	req := &ReportLogReq{
		Infos: []*ReportLogInfo{&ReportLogInfo{
			Servname: servname,
			Servid:   int32(m.Servid()),
			Stamp:    st["STAMP"],
			Cntrace:  st["TRACE"],
			Cndebug:  st["DEBUG"],
			Cninfo:   st["INFO"],
			Cnwarn:   st["WARN"],
			Cnerror:  st["ERROR"],
			Cnfatal:  st["FATAL"],
			Cnpanic:  st["PANIC"],
			Logs:     logs,
		}},
		Ip: m.ServIp(),
	}
	err := m.reporter.DoReport(ctx, req)
	if err != nil {
		xlog.Warn(ctx, err.Error())
	}
}

type Reporter interface {
	DoReport(context.Context, *ReportLogReq) error
}
