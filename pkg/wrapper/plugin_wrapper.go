// Copyright (c) 2022 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wrapper

import (
	"unsafe"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/tidwall/gjson"

	"github.com/mse-group/wasm-extensions-go/pkg/matcher"
)

type ParseConfigFunc[PluginConfig any] func(json gjson.Result, config *PluginConfig, log LogWrapper) error
type onHttpHeadersFunc[PluginConfig any] func(contextID uint32, config PluginConfig, needBody *bool, log LogWrapper) types.Action
type onHttpBodyFunc[PluginConfig any] func(contextID uint32, config PluginConfig, body []byte, log LogWrapper) types.Action

type CommonVmCtx[PluginConfig any] struct {
	types.DefaultVMContext
	pluginName            string
	parseConfig           ParseConfigFunc[PluginConfig]
	onHttpRequestHeaders  onHttpHeadersFunc[PluginConfig]
	onHttpRequestBody     onHttpBodyFunc[PluginConfig]
	onHttpResponseHeaders onHttpHeadersFunc[PluginConfig]
	onHttpResponseBody    onHttpBodyFunc[PluginConfig]
}

func SetCtx[PluginConfig any](pluginName string, setFuncs ...SetPluginFunc[PluginConfig]) {
	proxywasm.SetVMContext(NewCommonVmCtx(pluginName, setFuncs...))
}

type SetPluginFunc[PluginConfig any] func(*CommonVmCtx[PluginConfig])

func ParseConfigBy[PluginConfig any](f ParseConfigFunc[PluginConfig]) SetPluginFunc[PluginConfig] {
	return func(ctx *CommonVmCtx[PluginConfig]) {
		ctx.parseConfig = f
	}
}

func ProcessRequestHeadersBy[PluginConfig any](f onHttpHeadersFunc[PluginConfig]) SetPluginFunc[PluginConfig] {
	return func(ctx *CommonVmCtx[PluginConfig]) {
		ctx.onHttpRequestHeaders = f
	}
}

func ProcessRequestBodyBy[PluginConfig any](f onHttpBodyFunc[PluginConfig]) SetPluginFunc[PluginConfig] {
	return func(ctx *CommonVmCtx[PluginConfig]) {
		ctx.onHttpRequestBody = f
	}
}

func ProcessResponseHeadersBy[PluginConfig any](f onHttpHeadersFunc[PluginConfig]) SetPluginFunc[PluginConfig] {
	return func(ctx *CommonVmCtx[PluginConfig]) {
		ctx.onHttpResponseHeaders = f
	}
}

func ProcessResponseBodyBy[PluginConfig any](f onHttpBodyFunc[PluginConfig]) SetPluginFunc[PluginConfig] {
	return func(ctx *CommonVmCtx[PluginConfig]) {
		ctx.onHttpResponseBody = f
	}
}

func parseEmptyPluginConfig[PluginConfig any](gjson.Result, *PluginConfig, LogWrapper) error {
	return nil
}

func NewCommonVmCtx[PluginConfig any](pluginName string, setFuncs ...SetPluginFunc[PluginConfig]) *CommonVmCtx[PluginConfig] {
	ctx := &CommonVmCtx[PluginConfig]{
		pluginName: pluginName,
	}
	for _, set := range setFuncs {
		set(ctx)
	}
	if ctx.parseConfig == nil {
		var config PluginConfig
		if unsafe.Sizeof(config) != 0 {
			panic("the `parseConfig` is missing in NewCommonVmCtx's arguments")
		}
		ctx.parseConfig = parseEmptyPluginConfig[PluginConfig]
	}
	return ctx
}

func (ctx *CommonVmCtx[PluginConfig]) NewPluginContext(uint32) types.PluginContext {
	return &CommonPluginCtx[PluginConfig]{
		vm:  ctx,
		log: LogWrapper{ctx.pluginName},
	}
}

type CommonPluginCtx[PluginConfig any] struct {
	types.DefaultPluginContext
	matcher.RuleMatcher[PluginConfig]
	vm  *CommonVmCtx[PluginConfig]
	log LogWrapper
}

func (ctx *CommonPluginCtx[PluginConfig]) OnPluginStart(int) types.OnPluginStartStatus {
	data, err := proxywasm.GetPluginConfiguration()
	if err != nil && err != types.ErrorStatusNotFound {
		ctx.log.Criticalf("error reading plugin configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}
	if len(data) == 0 {
		ctx.log.Warn("need config")
		return types.OnPluginStartStatusFailed
	}
	if !gjson.ValidBytes(data) {
		ctx.log.Warnf("the plugin configuration is not a valid json: %s", string(data))
		return types.OnPluginStartStatusFailed

	}
	jsonData := gjson.ParseBytes(data)
	err = ctx.ParseRuleConfig(jsonData, func(js gjson.Result, cfg *PluginConfig) error {
		return ctx.vm.parseConfig(js, cfg, ctx.log)
	})
	if err != nil {
		ctx.log.Warnf("parse rule config failed: %v", err)
		return types.OnPluginStartStatusFailed
	}
	return types.OnPluginStartStatusOK
}

func (ctx *CommonPluginCtx[PluginConfig]) NewHttpContext(contextID uint32) types.HttpContext {
	httpCtx := &CommonHttpCtx[PluginConfig]{
		plugin:    ctx,
		contextID: contextID,
	}
	if ctx.vm.onHttpRequestBody != nil {
		httpCtx.needRequestBody = true
	}
	if ctx.vm.onHttpResponseBody != nil {
		httpCtx.needResponseBody = true
	}
	return httpCtx
}

type CommonHttpCtx[PluginConfig any] struct {
	types.DefaultHttpContext
	plugin           *CommonPluginCtx[PluginConfig]
	needRequestBody  bool
	needResponseBody bool
	requestBodySize  int
	responseBodySize int
	contextID        uint32
}

func (ctx *CommonHttpCtx[PluginConfig]) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
	if ctx.plugin.vm.onHttpRequestHeaders == nil {
		return types.ActionContinue
	}
	return ctx.plugin.Process(func(config PluginConfig) types.Action {
		return ctx.plugin.vm.onHttpRequestHeaders(ctx.contextID, config,
			&ctx.needRequestBody, ctx.plugin.log)
	})
}

func (ctx *CommonHttpCtx[PluginConfig]) OnHttpRequestBody(bodySize int, endOfStream bool) types.Action {
	if ctx.plugin.vm.onHttpRequestBody == nil {
		return types.ActionContinue
	}
	if !ctx.needRequestBody {
		return types.ActionContinue
	}
	ctx.requestBodySize += bodySize
	if !endOfStream {
		return types.ActionPause
	}
	body, err := proxywasm.GetHttpRequestBody(0, ctx.requestBodySize)
	if err != nil {
		ctx.plugin.log.Warnf("get request body failed: %v", err)
		return types.ActionContinue
	}
	return ctx.plugin.Process(func(config PluginConfig) types.Action {
		return ctx.plugin.vm.onHttpRequestBody(ctx.contextID, config, body, ctx.plugin.log)
	})
}

func (ctx *CommonHttpCtx[PluginConfig]) OnHttpResponseHeaders(numHeaders int, endOfStream bool) types.Action {
	if ctx.plugin.vm.onHttpResponseHeaders == nil {
		return types.ActionContinue
	}
	return ctx.plugin.Process(func(config PluginConfig) types.Action {
		return ctx.plugin.vm.onHttpResponseHeaders(ctx.contextID, config,
			&ctx.needResponseBody, ctx.plugin.log)
	})
}

func (ctx *CommonHttpCtx[PluginConfig]) OnHttpResponseBody(bodySize int, endOfStream bool) types.Action {
	if ctx.plugin.vm.onHttpResponseBody == nil {
		return types.ActionContinue
	}
	if !ctx.needResponseBody {
		return types.ActionContinue
	}
	ctx.responseBodySize += bodySize
	if !endOfStream {
		return types.ActionPause
	}
	body, err := proxywasm.GetHttpResponseBody(0, ctx.responseBodySize)
	if err != nil {
		ctx.plugin.log.Warnf("get response body failed: %v", err)
		return types.ActionContinue
	}
	return ctx.plugin.Process(func(config PluginConfig) types.Action {
		return ctx.plugin.vm.onHttpResponseBody(ctx.contextID, config, body, ctx.plugin.log)
	})
}
