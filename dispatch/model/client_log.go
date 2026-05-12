package model

import (
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ClientLog 客户端运行时日志（log8888 接口接收）
//
// 字段对应米哈游官方日志上报协议（不能改字段名 客户端按字段名上报）
// 包含设备信息（CPU/GPU/OS/型号）+ 错误信息（LogStr/StackTrace）+ 用户信息（Uid/UserName）
//
// 项目仅用 logger.Error 打印 不写入 DB
//
//	原因：客户端日志量大（每个错误都上报）写 DB 性能开销大
//	开发期通过日志查看就够 生产想保留可以加 DB 写入逻辑
type ClientLog struct {
	ID              primitive.ObjectID `json:"-" bson:"_id,omitempty"`
	Auid            string             `json:"auid" bson:"Auid"`
	ClientIp        string             `json:"clientIp" bson:"ClientIp"`
	CpuInfo         string             `json:"cpuInfo" bson:"CpuInfo"`
	DeviceModel     string             `json:"deviceModel" bson:"DeviceModel"`
	DeviceName      string             `json:"deviceName" bson:"DeviceName"`
	GpuInfo         string             `json:"gpuInfo" bson:"GpuInfo"`
	Guid            string             `json:"guid" bson:"Guid"`
	LogStr          string             `json:"logStr" bson:"LogStr"`
	LogType         string             `json:"logType" bson:"LogType"`
	OperatingSystem string             `json:"operatingSystem" bson:"OperatingSystem"`
	StackTrace      string             `json:"stackTrace" bson:"StackTrace"`
	Time            string             `json:"time" bson:"Time"`
	Uid             uint64             `json:"uid" bson:"Uid"`
	UserName        string             `json:"userName" bson:"UserName"`
	Version         string             `json:"version" bson:"Version"`
}
