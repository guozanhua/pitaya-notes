//
// Copyright (c) TFG Co. All Rights Reserved.
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/golang/protobuf/proto"

	"github.com/topfreegames/pitaya/agent"
	"github.com/topfreegames/pitaya/cluster"
	"github.com/topfreegames/pitaya/component"
	"github.com/topfreegames/pitaya/conn/codec"
	"github.com/topfreegames/pitaya/conn/message"
	"github.com/topfreegames/pitaya/constants"
	"github.com/topfreegames/pitaya/docgenerator"
	e "github.com/topfreegames/pitaya/errors"
	"github.com/topfreegames/pitaya/logger"
	"github.com/topfreegames/pitaya/protos"
	"github.com/topfreegames/pitaya/route"
	"github.com/topfreegames/pitaya/router"
	"github.com/topfreegames/pitaya/serialize"
	"github.com/topfreegames/pitaya/session"
	"github.com/topfreegames/pitaya/tracing"
	"github.com/topfreegames/pitaya/util"
)

// RemoteService struct 远程服务
type RemoteService struct {
	rpcServer              cluster.RPCServer             //rpcserver
	serviceDiscovery       cluster.ServiceDiscovery      //服务发现客户端
	serializer             serialize.Serializer          //序列化
	encoder                codec.PacketEncoder           //oacker 编码
	rpcClient              cluster.RPCClient             //rpc客户端
	services               map[string]*component.Service // all registered service
	router                 *router.Router                //路由器
	messageEncoder         message.Encoder               //message 编码
	server                 *cluster.Server               // server obj 远端服务器对象
	remoteBindingListeners []cluster.RemoteBindingListener
}

// NewRemoteService creates and return a new RemoteService
func NewRemoteService(
	rpcClient cluster.RPCClient,
	rpcServer cluster.RPCServer,
	sd cluster.ServiceDiscovery,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	router *router.Router,
	messageEncoder message.Encoder,
	server *cluster.Server,
) *RemoteService {
	return &RemoteService{
		services:               make(map[string]*component.Service),
		rpcClient:              rpcClient,
		rpcServer:              rpcServer,
		encoder:                encoder,
		serviceDiscovery:       sd,
		serializer:             serializer,
		router:                 router,
		messageEncoder:         messageEncoder,
		server:                 server,
		remoteBindingListeners: make([]cluster.RemoteBindingListener, 0),
	}
}

var remotes = make(map[string]*component.Remote) // all remote method   所有远程方法调用的反射信息

// 处理rpc调用根据消息类型进行回复，此处可以是client发送给后端服务器的消息 前端服务器进行的一个转发 转发使用rpc调用
func (r *RemoteService) remoteProcess(
	ctx context.Context,
	server *cluster.Server,
	a *agent.Agent,
	route *route.Route,
	msg *message.Message,
) {
	res, err := r.remoteCall(ctx, server, protos.RPCType_Sys, route, a.Session, msg)
	switch msg.Type {
	case message.Request: //请求则通过session返回res
		if err != nil {
			logger.Log.Errorf("Failed to process remote: %s", err.Error())
			a.AnswerWithError(ctx, msg.ID, err)
			return
		}
		err := a.Session.ResponseMID(ctx, msg.ID, res.Data)
		if err != nil {
			logger.Log.Errorf("Failed to respond remote: %s", err.Error())
			a.AnswerWithError(ctx, msg.ID, err)
		}
	case message.Notify:
		defer tracing.FinishSpan(ctx, err)
		if err == nil && res.Error != nil {
			err = errors.New(res.Error.GetMsg())
		}
		if err != nil {
			logger.Log.Errorf("error while sending a notify: %s", err.Error())
		}
	}
}

// AddRemoteBindingListener adds a listener
func (r *RemoteService) AddRemoteBindingListener(bindingListener cluster.RemoteBindingListener) {
	r.remoteBindingListeners = append(r.remoteBindingListeners, bindingListener)
}

// Call processes a remote call rpc客户端的方法调用
func (r *RemoteService) Call(ctx context.Context, req *protos.Request) (*protos.Response, error) {
	c, err := util.GetContextFromRequest(req, r.server.ID)
	c = util.StartSpanFromRequest(c, r.server.ID, req.GetMsg().GetRoute())
	var res *protos.Response
	if err != nil {
		res = &protos.Response{
			Error: &protos.Error{
				Code: e.ErrInternalCode,
				Msg:  err.Error(),
			},
		}
	} else {
		res = processRemoteMessage(c, req, r)
	}

	if res.Error != nil {
		err = errors.New(res.Error.Msg)
	}

	defer tracing.FinishSpan(c, err)
	return res, nil
}

// SessionBindRemote is called when a remote server binds a user session and want us to acknowledge it
func (r *RemoteService) SessionBindRemote(ctx context.Context, msg *protos.BindMsg) (*protos.Response, error) {
	for _, r := range r.remoteBindingListeners {
		r.OnUserBind(msg.Uid, msg.Fid)
	}
	return &protos.Response{
		Data: []byte("ack"),
	}, nil
}

// PushToUser sends a push to user
func (r *RemoteService) PushToUser(ctx context.Context, push *protos.Push) (*protos.Response, error) {
	logger.Log.Debugf("sending push to user %s: %v", push.GetUid(), string(push.Data))
	s := session.GetSessionByUID(push.GetUid())
	if s != nil {
		err := s.Push(push.Route, push.Data)
		if err != nil {
			return nil, err
		}
		return &protos.Response{
			Data: []byte("ack"),
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// KickUser sends a kick to user
func (r *RemoteService) KickUser(ctx context.Context, kick *protos.KickMsg) (*protos.KickAnswer, error) {
	logger.Log.Debugf("sending kick to user %s", kick.GetUserId())
	s := session.GetSessionByUID(kick.GetUserId())
	if s != nil {
		err := s.Kick(ctx)
		if err != nil {
			return nil, err
		}
		return &protos.KickAnswer{
			Kicked: true,
		}, nil
	}
	return nil, constants.ErrSessionNotFound
}

// DoRPC do rpc and get answer 调用指定id服务器的rpc
func (r *RemoteService) DoRPC(ctx context.Context, serverID string, route *route.Route, protoData []byte) (*protos.Response, error) {
	msg := &message.Message{
		Type:  message.Request,
		Route: route.Short(),
		Data:  protoData,
	}

	target, _ := r.serviceDiscovery.GetServer(serverID)
	if serverID != "" && target == nil {
		return nil, constants.ErrServerNotFound
	}

	//rpc rpc to->rpc->do prc RPCType_User
	return r.remoteCall(ctx, target, protos.RPCType_User, route, nil, msg)
}

// RPC makes rpcs
// RPC或者RPCTo通过doRPC调用过来的 user rpc调用
func (r *RemoteService) RPC(ctx context.Context, serverID string, route *route.Route, reply proto.Message, arg proto.Message) error {
	var data []byte
	var err error
	if arg != nil {
		data, err = proto.Marshal(arg)
		if err != nil {
			return err
		}
	}
	res, err := r.DoRPC(ctx, serverID, route, data)
	if err != nil {
		return err
	}

	if res.Error != nil {
		return &e.Error{
			Code:     res.Error.Code,
			Message:  res.Error.Msg,
			Metadata: res.Error.Metadata,
		}
	}

	if reply != nil {
		err = proto.Unmarshal(res.GetData(), reply)
		if err != nil {
			return err
		}
	}
	return nil
}

// Register registers components
func (r *RemoteService) Register(comp component.Component, opts []component.Option) error {
	//组件生成Service
	s := component.NewService(comp, opts)

	if _, ok := r.services[s.Name]; ok {
		return fmt.Errorf("remote: service already defined: %s", s.Name)
	}

	//遍历组件方法取出符合rpc调用的方法封装为Remote 并且存储为method-name:Remote的map
	if err := s.ExtractRemote(); err != nil {
		return err
	}

	//保存Service
	r.services[s.Name] = s
	// register all remotes
	// 保存所有的Remote
	for name, remote := range s.Remotes {
		remotes[fmt.Sprintf("%s.%s", s.Name, name)] = remote
	}

	return nil
}

//processRemoteMessage 处理远程消息 sys和user rpc
func processRemoteMessage(ctx context.Context, req *protos.Request, r *RemoteService) *protos.Response {
	rt, err := route.Decode(req.GetMsg().GetRoute())
	if err != nil {
		response := &protos.Response{
			Error: &protos.Error{
				Code: e.ErrBadRequestCode,
				Msg:  "cannot decode route",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
		return response
	}

	switch {
	case req.Type == protos.RPCType_Sys:
		return r.handleRPCSys(ctx, req, rt) //系统rpc调用 本地handlers中的handler进行消息处理
	case req.Type == protos.RPCType_User:
		return r.handleRPCUser(ctx, req, rt) //用户rpc 使用remtoes中的remote进行消息处理
	default:
		return &protos.Response{
			Error: &protos.Error{
				Code: e.ErrBadRequestCode,
				Msg:  "invalid rpc type",
				Metadata: map[string]string{
					"route": req.GetMsg().GetRoute(),
				},
			},
		}
	}
}

func (r *RemoteService) handleRPCUser(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	//构建一个rpc返回消息
	response := &protos.Response{}

	remote, ok := remotes[rt.Short()] //根据路由获取远程调用的方法反射信息
	if !ok {
		logger.Log.Warnf("pitaya/remote: %s not found", rt.Short())
		response := &protos.Response{
			Error: &protos.Error{
				Code: e.ErrNotFoundCode,
				Msg:  "route not found",
				Metadata: map[string]string{
					"route": rt.Short(),
				},
			},
		}
		return response
	}
	//构建调用参数 ctx msg
	params := []reflect.Value{remote.Receiver, reflect.ValueOf(ctx)}
	if remote.HasArgs {
		arg, err := unmarshalRemoteArg(remote, req.GetMsg().GetData())
		if err != nil {
			response := &protos.Response{
				Error: &protos.Error{
					Code: e.ErrBadRequestCode,
					Msg:  err.Error(),
				},
			}
			return response
		}
		params = append(params, reflect.ValueOf(arg))
	}

	//方法调用
	ret, err := util.Pcall(remote.Method, params)
	if err != nil {
		response := &protos.Response{
			Error: &protos.Error{
				Code: e.ErrUnknownCode,
				Msg:  err.Error(),
			},
		}
		if val, ok := err.(*e.Error); ok {
			response.Error.Code = val.Code
			if val.Metadata != nil {
				response.Error.Metadata = val.Metadata
			}
		}
		return response
	}

	var b []byte
	if ret != nil {
		pb, ok := ret.(proto.Message)
		if !ok {
			response := &protos.Response{
				Error: &protos.Error{
					Code: e.ErrUnknownCode,
					Msg:  constants.ErrWrongValueType.Error(),
				},
			}
			return response
		}
		if b, err = proto.Marshal(pb); err != nil {
			response := &protos.Response{
				Error: &protos.Error{
					Code: e.ErrUnknownCode,
					Msg:  err.Error(),
				},
			}
			return response
		}
	}

	response.Data = b
	return response
}

//handleRPCSys系统rpc 调用的是本地消息
func (r *RemoteService) handleRPCSys(ctx context.Context, req *protos.Request, rt *route.Route) *protos.Response {
	reply := req.GetMsg().GetReply()
	response := &protos.Response{}
	// (warning) a new agent is created for every new request
	a, err := agent.NewRemote(
		req.GetSession(),
		reply,
		r.rpcClient,
		r.encoder,
		r.serializer,
		r.serviceDiscovery,
		req.FrontendID,
		r.messageEncoder,
	)
	if err != nil {
		logger.Log.Warn("pitaya/handler: cannot instantiate remote agent")
		response := &protos.Response{
			Error: &protos.Error{
				Code: e.ErrInternalCode,
				Msg:  err.Error(),
			},
		}
		return response
	}

	//根据路由信息进行消息处理
	ret, err := processHandlerMessage(ctx, rt, r.serializer, a.Session, req.GetMsg().GetData(), req.GetMsg().GetType(), true)
	if err != nil {
		logger.Log.Warnf(err.Error())
		response = &protos.Response{
			Error: &protos.Error{
				Code: e.ErrUnknownCode,
				Msg:  err.Error(),
			},
		}
		if val, ok := err.(*e.Error); ok {
			response.Error.Code = val.Code
			if val.Metadata != nil {
				response.Error.Metadata = val.Metadata
			}
		}
	} else {
		response = &protos.Response{Data: ret}
	}
	return response
}

// remoteCall rpc调用 路由到指定的server调用rpcClient根军server找到连接server的grprc client进行rpc调用，最终调用到remoteSevice的Call方法使用handlers或者remotes进行处理
func (r *RemoteService) remoteCall(
	ctx context.Context,
	server *cluster.Server,
	rpcType protos.RPCType,
	route *route.Route,
	session *session.Session,
	msg *message.Message,
) (*protos.Response, error) {
	svType := route.SvType

	var err error
	target := server

	//远程目标服务器为空则重新使用路由函数获取一个服务器
	if target == nil {
		target, err = r.router.Route(ctx, rpcType, svType, route, msg)
		if err != nil {
			return nil, e.NewError(err, e.ErrInternalCode)
		}
	}

	//rpc调用远程服务器
	res, err := r.rpcClient.Call(ctx, rpcType, route, session, msg, target)
	if err != nil {
		return nil, err
	}
	return res, err
}

// DumpServices outputs all registered services
func (r *RemoteService) DumpServices() {
	for name := range remotes {
		logger.Log.Infof("registered remote %s", name)
	}
}

// Docs returns documentation for remotes
func (r *RemoteService) Docs(getPtrNames bool) (map[string]interface{}, error) {
	if r == nil {
		return map[string]interface{}{}, nil
	}
	return docgenerator.RemotesDocs(r.server.Type, r.services, getPtrNames)
}
