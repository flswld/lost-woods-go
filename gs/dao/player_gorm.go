package dao

// MySQL/SQLite GORM 实现 - 玩家档持久化层（DB 三选一之一）
//
// **存储策略：玩家档作为 BLOB 整体存**
//   - 不展平字段到列（避免 schema 与玩家档结构耦合 修改 db_player.go 无需迁移表）
//   - msgpack 序列化整个 Player 对象 → longblob 列
//   - 反序列化时 player.PlayerId 字段不在 BLOB 里（uid 是主键列）需手动回填
//
// 三张表（启动时 AutoMigrate 自动建表 dao/dao.go:114）：
//   1. player(uid, data)              玩家档 主键 uid
//   2. chat_msg(...)                  聊天记录 字段展平存（需要按 uid+to_uid 查询 + Sequence/Time 索引 不能整存 BLOB）
//   3. scene_block(uid, block_id, data)  场景区块存档 玩家进度按 block 切片
//
// **逻辑删除**：chat_msg.is_delete + DeleteUpdateChatMsgByUid 标记删除 不真删
//   原因：跨服私聊 / 玩家档清档时需要保留消息历史（防止对方一方删除影响双方）
//
// 与 player_mongo.go 的关系：dao.go:80-122 根据 database.url 前缀选实现
//   mysql:// / sqlite:// → 走这里
//   mongodb://           → 走 player_mongo.go
//   InsertPlayer/UpdatePlayer 等顶层方法在 mongo.go 内分支判断 d.mongo==nil → 转发到这里

import (
	"errors"

	"hk4e/gs/model"

	"github.com/vmihailenco/msgpack/v5"
	"gorm.io/gorm"
)

// PlayerGorm 玩家档表 整存 msgpack 字节
// uid 主键 多 GS 部署时玩家也只在一行（跨服迁移走"读出来再 Update"）
type PlayerGorm struct {
	Uid  uint32 `gorm:"column:uid;type:bigint(20);primaryKey"`
	Data []byte `gorm:"column:data;type:longblob"` // msgpack(Player) 整对象
}

func (p PlayerGorm) TableName() string {
	return "player"
}

// ChatMsgGorm 私聊消息表 字段展平存（不像玩家档那样整存）
//
// 原因：需要按 to_uid/uid + time 查询历史 + 按 sequence 增量拉取 + 按 is_read 标记已读
// 整存 BLOB 会导致这些查询无法走索引必须全表扫
//
// 跨服私聊也写在这张表（按 to_uid 查询时会同时拿到双向消息）
type ChatMsgGorm struct {
	ID       uint32 `gorm:"column:id;type:bigint(20);primaryKey;autoIncrement"`
	Sequence uint32 `gorm:"column:sequence;type:bigint(20)"`  // 客户端增量拉取序号（从 101 开始 1-100 留给系统消息）
	Time     uint32 `gorm:"column:time;type:bigint(20)"`      // 发送时间 Unix 秒
	Uid      uint32 `gorm:"column:uid;type:bigint(20)"`       // 发送者 uid
	ToUid    uint32 `gorm:"column:to_uid;type:bigint(20)"`    // 接收者 uid
	IsRead   bool   `gorm:"column:is_read;type:tinyint(1)"`   // 已读标记
	MsgType  uint8  `gorm:"column:msg_type;type:tinyint(1)"`  // 0=文本 1=表情 (constant.ChatMsgType*)
	Text     string `gorm:"column:text;type:text"`            // 文本内容（含 GM 命令）
	Icon     uint32 `gorm:"column:icon;type:bigint(20)"`      // 表情 id（MsgType=1 时用）
	IsDelete bool   `gorm:"column:is_delete;type:tinyint(1)"` // 逻辑删除 不真删（防双方消息历史不一致）
}

func (c ChatMsgGorm) TableName() string {
	return "chat_msg"
}

// SceneBlockGorm 场景区块存档表 整存 msgpack 字节
//
// 玩家进度按 block 切片：每个 block 单独一行（uid + block_id 复合查询）
// 这样进场景时只需加载玩家附近的 block 不必加载全场景（玩家可能有上千 block 进度）
//
// 注意没有 primaryKey 标记 实际上 (uid, block_id) 应该是联合主键
// 但 GORM AutoMigrate 不会自动建联合索引 大数据量时可能有性能问题
type SceneBlockGorm struct {
	Uid     uint32 `gorm:"column:uid;type:bigint(20)"`
	BlockId uint32 `gorm:"column:block_id;type:bigint(20)"`
	Data    []byte `gorm:"column:data;type:longblob"` // msgpack(SceneBlock) 整对象
}

func (s SceneBlockGorm) TableName() string {
	return "scene_block"
}

func (d *Dao) InsertPlayerGorm(player *model.Player) error {
	data, err := msgpack.Marshal(player)
	if err != nil {
		return err
	}
	err = d.gormDb.Create(&PlayerGorm{
		Uid:  player.PlayerId,
		Data: data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) InsertPlayerListGorm(playerList []*model.Player) error {
	for _, player := range playerList {
		err := d.InsertPlayerGorm(player)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Dao) DeletePlayerGorm(playerId uint32) error {
	d.gormDb.Where("uid = ?", playerId).Delete(&PlayerGorm{})
	return nil
}

func (d *Dao) DeletePlayerListGorm(playerIdList []uint32) error {
	for _, playerId := range playerIdList {
		err := d.DeletePlayerGorm(playerId)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Dao) UpdatePlayerGorm(player *model.Player) error {
	data, err := msgpack.Marshal(player)
	if err != nil {
		return err
	}
	err = d.gormDb.Updates(&PlayerGorm{
		Uid:  player.PlayerId,
		Data: data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) UpdatePlayerListGorm(playerList []*model.Player) error {
	for _, player := range playerList {
		err := d.UpdatePlayerGorm(player)
		if err != nil {
			return err
		}
	}
	return nil
}

// QueryPlayerByIdGorm 按 uid 查玩家档 返回 nil 表示玩家不存在（非错误）
// 注意：PlayerId 字段不在 msgpack BLOB 里（id 是主键列） 反序列化后需手动回填
func (d *Dao) QueryPlayerByIdGorm(playerId uint32) (*model.Player, error) {
	playerGorm := new(PlayerGorm)
	err := d.gormDb.Where("uid = ?", playerId).First(playerGorm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	player := new(model.Player)
	err = msgpack.Unmarshal(playerGorm.Data, player)
	if err != nil {
		return nil, err
	}
	player.PlayerId = playerId
	return player, nil
}

func (d *Dao) QueryPlayerListGorm() ([]*model.Player, error) {
	var playerGormList []*PlayerGorm = nil
	err := d.gormDb.Find(&playerGormList).Error
	if err != nil {
		return nil, err
	}
	playerList := make([]*model.Player, 0)
	for _, playerGorm := range playerGormList {
		player := new(model.Player)
		err = msgpack.Unmarshal(playerGorm.Data, player)
		if err != nil {
			return nil, err
		}
		playerList = append(playerList, player)
	}
	return playerList, nil
}

func (d *Dao) InsertChatMsgGorm(chatMsg *model.ChatMsg) error {
	err := d.gormDb.Create(&ChatMsgGorm{
		Sequence: chatMsg.Sequence,
		Time:     chatMsg.Time,
		Uid:      chatMsg.Uid,
		ToUid:    chatMsg.ToUid,
		IsRead:   chatMsg.IsRead,
		MsgType:  chatMsg.MsgType,
		Text:     chatMsg.Text,
		Icon:     chatMsg.Icon,
		IsDelete: chatMsg.IsDelete,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) DeleteUpdateChatMsgByUidGorm(uid uint32) error {
	err := d.gormDb.Model(&ChatMsgGorm{}).Where("to_uid = ? or uid = ?", uid, uid).Update("is_delete", true).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) UpdateChatMsgByUidAndToUidActionReadGorm(uid uint32, toUid uint32) error {
	err := d.gormDb.Model(&ChatMsgGorm{}).Where("to_uid = ? and uid = ?", uid, toUid).Update("is_read", true).Error
	if err != nil {
		return err
	}
	return nil
}

// QueryChatMsgListByUidGorm 查询某玩家所有相关私聊（双向 收+发）
//
// **SQL 优先级修复**：原版 `a or b and c` 在 SQL 里实际是 `a or (b and c)`
//
//	导致 to_uid==uid 时不过滤 is_delete 拉出对方已删除的消息
//	现在用 `(a or b) and c` 显式分组 与 Mongo 版语义一致
//
// 上限 MaxQueryChatMsgLen=1000 防止超长聊天记录拖垮 DB
// 加载后由 LoadUserChatMsgFromDbSync 按 100 条上限+sequence 重排（player_chat.go MaxMsgListLen）
func (d *Dao) QueryChatMsgListByUidGorm(uid uint32) ([]*model.ChatMsg, error) {
	var chatMsgGormList []*ChatMsgGorm = nil
	err := d.gormDb.Where("(to_uid = ? or uid = ?) and is_delete = ?", uid, uid, false).Find(&chatMsgGormList).
		Order("time DESC").Limit(MaxQueryChatMsgLen).Error
	if err != nil {
		return nil, err
	}
	chatMsgList := make([]*model.ChatMsg, 0)
	for _, chatMsgGorm := range chatMsgGormList {
		chatMsgList = append(chatMsgList, &model.ChatMsg{
			Sequence: chatMsgGorm.Sequence,
			Time:     chatMsgGorm.Time,
			Uid:      chatMsgGorm.Uid,
			ToUid:    chatMsgGorm.ToUid,
			IsRead:   chatMsgGorm.IsRead,
			MsgType:  chatMsgGorm.MsgType,
			Text:     chatMsgGorm.Text,
			Icon:     chatMsgGorm.Icon,
			IsDelete: chatMsgGorm.IsDelete,
		})
	}
	return chatMsgList, nil
}

func (d *Dao) InsertSceneBlockGorm(sceneBlock *model.SceneBlock) error {
	data, err := msgpack.Marshal(sceneBlock)
	if err != nil {
		return err
	}
	err = d.gormDb.Create(&SceneBlockGorm{
		Uid:     sceneBlock.Uid,
		BlockId: sceneBlock.BlockId,
		Data:    data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) UpdateSceneBlockGorm(sceneBlock *model.SceneBlock) error {
	data, err := msgpack.Marshal(sceneBlock)
	if err != nil {
		return err
	}
	err = d.gormDb.Updates(&SceneBlockGorm{
		Uid:     sceneBlock.Uid,
		BlockId: sceneBlock.BlockId,
		Data:    data,
	}).Error
	if err != nil {
		return err
	}
	return nil
}

func (d *Dao) QuerySceneBlockByUidAndBlockIdGorm(uid uint32, blockId uint32) (*model.SceneBlock, error) {
	sceneBlockGorm := new(SceneBlockGorm)
	err := d.gormDb.Where("uid = ? and block_id = ?", uid, blockId).First(sceneBlockGorm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	sceneBlock := new(model.SceneBlock)
	err = msgpack.Unmarshal(sceneBlockGorm.Data, sceneBlock)
	if err != nil {
		return nil, err
	}
	return sceneBlock, nil
}
