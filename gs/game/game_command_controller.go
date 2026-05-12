package game

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"hk4e/common/constant"
	"hk4e/gdconf"

	"github.com/flswld/halo/logger"
)

// 玩家游戏内 GM 命令实现集合（聊天命令）
//
// 本文件包含 25 个 GM 命令的具体实现 全部走 PlayerChatGM 入口
// 玩家私聊"小可爱" → 命令文本（如"give 1234 5"）解析 → 调对应的 XxxCommand 函数
//
// 已实现的命令（按 InitController 注册顺序）：
//   - assign: 指定 GM 命令的目标玩家（"@玩家 命令" 模式）
//   - help: 命令帮助文档（普通玩家也能用）
//   - wudi/energy/stamina/nocd: 无敌 / 无限能量 / 无限体力 / 无技能CD（开关）
//   - goto/jump: 传送（按场景id+坐标 / 按传送点id）
//   - avatar/equip: 添加角色 / 装备
//   - item: 添加物品（含原石/摩拉/树脂等虚拟物品）
//   - kill/monster/gadget: 杀实体 / 刷怪 / 刷物件
//   - quest: 接/完成/跳过任务
//   - point/area: 解锁传送点 / 区域
//   - weather: 设置天气
//   - openstate: 设置游戏功能开放状态（解锁角色卡池/秘境/活动等）
//   - talent: 解锁角色命之座
//   - player/level/break: 玩家信息 / 等级 / 突破
//   - clear: 清档（删档重练）
//   - debug: 调试命令（开发用）

// CommandController 命令控制器（每个命令一个）
//
// 字段：
//   - Name: 命令中文名（如"无敌"）
//   - AliasList: 命令别名列表（如["wudi"]）多个别名都能调起
//   - Description: 帮助信息描述（带颜色标签）
//   - UsageList: 用法示例列表（{alias} 占位符运行时替换为实际命令名）
//   - Perm: 权限要求（CommandPermNormal / CommandPermGM）
//   - Func: 实际命令实现函数
type CommandController struct {
	Name        string      // 名称
	AliasList   []string    // 别名列表
	Description string      // 命令描述
	UsageList   []string    // 用法描述
	Perm        CommandPerm // 权限
	Func        CommandFunc // 命令执行函数
}

// InitController 初始化命令控制器
func (c *CommandManager) InitController() {
	controllerList := []*CommandController{
		c.NewAssignCommandController(),
		c.NewHelpCommandController(),
		c.NewWudiCommandController(),
		c.NewEnergyCommandController(),
		c.NewStaminaCommandController(),
		c.NewNoCdCommandController(),
		c.NewGotoCommandController(),
		c.NewJumpCommandController(),
		c.NewAvatarCommandController(),
		c.NewEquipCommandController(),
		c.NewItemCommandController(),
		c.NewKillCommandController(),
		c.NewMonsterCommandController(),
		c.NewGadgetCommandController(),
		c.NewQuestCommandController(),
		c.NewPointCommandController(),
		c.NewAreaCommandController(),
		c.NewWeatherCommandController(),
		c.NewOpenStateCommandController(),
		c.NewTalentCommandController(),
		c.NewPlayerCommandController(),
		c.NewLevelCommandController(),
		c.NewBreakCommandController(),
		c.NewClearCommandController(),
		c.NewDebugCommandController(),
	}
	c.RegAllController(controllerList...)
}

// 指定命令

func (c *CommandManager) NewAssignCommandController() *CommandController {
	return &CommandController{
		Name:        "指定",
		AliasList:   []string{"assign"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>设置命令指定玩家</color>",
		UsageList: []string{
			"{alias} <目标UID> 指定某个玩家",
		},
		Perm: CommandPermGM,
		Func: c.AssignCommand,
	}
}

func (c *CommandManager) AssignCommand(content *CommandContent) bool {
	var assignUid uint32

	return content.Must("uint32", func(p any) bool {
		value := p.(uint32)
		// 指定uid
		assignUid = value
		return true
	}).Execute(func() bool {
		// 设置命令指定uid
		content.Executor.CommandAssignUid = assignUid
		content.SendSuccMessage(content.Executor, "已指定玩家，指定UID：%v", assignUid)
		return true
	})
}

// 帮助命令

func (c *CommandManager) NewHelpCommandController() *CommandController {
	return &CommandController{
		Name:        "帮助",
		AliasList:   []string{"help"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>帮助</color>",
		UsageList: []string{
			"{alias} 查看简要帮助信息",
			"{alias} <页数> 查看指定页数的帮助信息",
			"{alias} <命令别名> 查看详细帮助信息",
		},
		Perm: CommandPermNormal,
		Func: c.HelpCommand,
	}
}

func (c *CommandManager) HelpCommand(content *CommandContent) bool {
	var mode = "page"                 // 模式
	var page uint32 = 1               // 页数
	var alias string                  // 别名
	var controller *CommandController // 命令控制器

	return content.Option("string", func(p any) bool {
		value := p.(string)
		// 数字的话就是页面
		parseUint, err := strconv.ParseUint(value, 10, 32)
		if err == nil {
			page = uint32(parseUint)
			mode = "page"
			return true
		}
		// 通过别名获取
		controller = c.commandControllerMap[value]
		if controller == nil {
			return false
		}
		alias = value
		mode = "alias"
		return true
	}).Execute(func() bool {
		switch mode {
		case "page":
			// 显示简要帮助信息
			helpText := "<color=#66B2FF>================</color><color=#CCE5FF>/ 帮 助 /</color><color=#66B2FF>================</color>\n"
			// 获取玩家权限足够的命令列表
			playerCommandControllerList := make([]*CommandController, 0)
			for _, commandController := range c.commandControllerList {
				// 权限不足跳过
				if content.Executor.CmdPerm < uint8(commandController.Perm) {
					continue
				}
				playerCommandControllerList = append(playerCommandControllerList, commandController)
			}
			// 每页显示的命令数量
			const commandsPerPage = 10
			// 最大页数
			maxPages := uint32(math.Ceil(float64(len(playerCommandControllerList)) / float64(commandsPerPage)))
			// 页数超出范围
			if page > maxPages {
				content.SendFailMessage(content.AssignPlayer, "超出命令帮助页数范围，最大页数：%v", maxPages)
				return true
			}
			// 获取页数索引
			startIndex := int((page - 1) * commandsPerPage)
			endIndex := startIndex + commandsPerPage
			if endIndex > len(playerCommandControllerList) {
				endIndex = len(playerCommandControllerList)
			}
			// 添加帮助文本
			for i, commandController := range playerCommandControllerList[startIndex:endIndex] {
				// 计算命令在整个列表中的索引
				commandIndex := startIndex + i + 1
				// GM命令和普通命令区分颜色
				var permColor string
				switch commandController.Perm {
				case CommandPermNormal:
					permColor = "#CCFFCC"
				case CommandPermGM:
					permColor = "#FF9999"
				}
				helpText += fmt.Sprintf("<color=%v>%v. %v</color> <color=#FFE5CC>-</color> %v\n", permColor, commandIndex, commandController.Name, strings.ReplaceAll(commandController.Description, "{alias}", commandController.AliasList[0]))
			}
			helpText += fmt.Sprintf("\n<color=#CCE5FF>当前第 %v 页，共 %v 页，help命令后加页码翻页~</color>", page, maxPages)
			// 发送帮助文本
			content.SendMessage(content.Executor, helpText)
			return true
		case "alias":
			// 命令详细用法
			usage := "命令用法：\n"
			for i, s := range controller.UsageList {
				s = strings.ReplaceAll(s, "{alias}", alias)
				usage += fmt.Sprintf("%v. %v", i+1, s)
				// 换行
				if i != len(controller.UsageList)-1 {
					usage += "\n"
				}
			}
			text := fmt.Sprintf("<color=#FFFFCC>%v</color><color=#CCCCFF> 命令详细帮助：</color>\n\n%v\n\n<color=#CCE5FF>所有别名：</color><color=#E0E0E0>%v</color>", controller.Name, usage, controller.AliasList)
			content.SendMessage(content.Executor, text)
			return true
		}
		return false
	})
}

// 无敌命令

func (c *CommandManager) NewWudiCommandController() *CommandController {
	return &CommandController{
		Name:        "无敌",
		AliasList:   []string{"wudi"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>无敌</color>",
		UsageList: []string{
			"{alias} global avatar <on/off> 开关玩家无敌",
			"{alias} global monster <on/off> 开关怪物无敌",
		},
		Perm: CommandPermNormal,
		Func: c.WudiCommand,
	}
}

func (c *CommandManager) WudiCommand(content *CommandContent) bool {
	var mode1 string // 模式1
	var mode2 string // 模式2
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode1 = p.(string)
		return true
	}).Must("string", func(p any) bool {
		mode2 = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode1 {
		case "global":
			switch mode2 {
			case "avatar":
				switch param {
				case "on":
					c.gmCmd.GMSetPlayerWuDi(content.AssignPlayer.PlayerId, true)
					content.SendSuccMessage(content.Executor, "已开启玩家无敌，指定UID：%v。", content.AssignPlayer.PlayerId)
					return true
				case "off":
					c.gmCmd.GMSetPlayerWuDi(content.AssignPlayer.PlayerId, false)
					content.SendSuccMessage(content.Executor, "已关闭玩家无敌，指定UID：%v。", content.AssignPlayer.PlayerId)
					return true
				default:
					return false
				}
			case "monster":
				switch param {
				case "on":
					c.gmCmd.GMSetMonsterWudi(content.AssignPlayer.PlayerId, true)
					content.SendSuccMessage(content.Executor, "已开启怪物无敌，指定UID：%v。", content.AssignPlayer.PlayerId)
					return true
				case "off":
					c.gmCmd.GMSetMonsterWudi(content.AssignPlayer.PlayerId, false)
					content.SendSuccMessage(content.Executor, "已关闭怪物无敌，指定UID：%v。", content.AssignPlayer.PlayerId)
					return true
				default:
					return false
				}
			default:
				return false
			}
		default:
			return false
		}
	})
}

// 元素能量命令

func (c *CommandManager) NewEnergyCommandController() *CommandController {
	return &CommandController{
		Name:        "元素能量",
		AliasList:   []string{"energy"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>元素能量</color>",
		UsageList: []string{
			"{alias} infinite <on/off> 开关无限元素爆发",
		},
		Perm: CommandPermNormal,
		Func: c.EnergyCommand,
	}
}

func (c *CommandManager) EnergyCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "infinite":
			switch param {
			case "on":
				c.gmCmd.GMSetPlayerEnergyInf(content.AssignPlayer.PlayerId, true)
				content.SendSuccMessage(content.Executor, "已开启无限元素爆发，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			case "off":
				c.gmCmd.GMSetPlayerEnergyInf(content.AssignPlayer.PlayerId, false)
				content.SendSuccMessage(content.Executor, "已关闭无限元素爆发，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			default:
				return false
			}
		default:
			return false
		}
	})
}

// 耐力命令

func (c *CommandManager) NewStaminaCommandController() *CommandController {
	return &CommandController{
		Name:        "耐力",
		AliasList:   []string{"stamina"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>耐力</color>",
		UsageList: []string{
			"{alias} infinite <on/off> 开关无限耐力",
		},
		Perm: CommandPermNormal,
		Func: c.StaminaCommand,
	}
}

func (c *CommandManager) StaminaCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "infinite":
			switch param {
			case "on":
				c.gmCmd.GMSetPlayerStaminaInf(content.AssignPlayer.PlayerId, true)
				content.SendSuccMessage(content.Executor, "已开启无限耐力，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			case "off":
				c.gmCmd.GMSetPlayerStaminaInf(content.AssignPlayer.PlayerId, false)
				content.SendSuccMessage(content.Executor, "已关闭无限耐力，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			default:
				return false
			}
		default:
			return false
		}
	})
}

// 无冷却命令

func (c *CommandManager) NewNoCdCommandController() *CommandController {
	return &CommandController{
		Name:        "无冷却",
		AliasList:   []string{"nocd"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>无冷却</color>",
		UsageList: []string{
			"{alias} <on/off> 开关无冷却",
		},
		Perm: CommandPermNormal,
		Func: c.NoCdCommand,
	}
}

func (c *CommandManager) NoCdCommand(content *CommandContent) bool {
	var param string // 参数

	return content.Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch param {
		case "on":
			c.gmCmd.GMSetPlayerNoCd(content.AssignPlayer.PlayerId, true)
			content.SendSuccMessage(content.Executor, "已开启无冷却，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "off":
			c.gmCmd.GMSetPlayerNoCd(content.AssignPlayer.PlayerId, false)
			content.SendSuccMessage(content.Executor, "已关闭无冷却，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		default:
			return false
		}
	})
}

// 传送坐标命令

func (c *CommandManager) NewGotoCommandController() *CommandController {
	return &CommandController{
		Name:        "传送坐标",
		AliasList:   []string{"goto"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>传送到指定坐标</color>",
		UsageList: []string{
			"{alias} <坐标X> <坐标Y> <坐标Z> 传送至指定坐标",
		},
		Perm: CommandPermNormal,
		Func: c.GotoCommand,
	}
}

func (c *CommandManager) GotoCommand(content *CommandContent) bool {
	// 计算相对坐标
	parseRelativePosFunc := func(param string, pos float64) (float64, bool) {
		// 不以 ~ 开头代表使用绝对坐标
		if !strings.HasPrefix(param, "~") {
			value, err := strconv.ParseFloat(param, 64)
			return value, err == nil
		}
		// 用户只输入 ~ 获取为玩家当前位置
		if param == "~" {
			return pos, true
		}
		// 以 ~ 开头 此时位置加 ~ 后的数
		param = param[1:] // 去除 ~
		addPos, err := strconv.ParseFloat(param, 64)
		if err != nil {
			return 0, false
		}
		// 计算坐标
		pos += addPos
		return pos, true
	}
	// 传送玩家到场景以及坐标
	var sceneId = content.AssignPlayer.GetSceneId()
	var posX, posY, posZ float64

	// 解析命令
	playerPos := GAME.GetPlayerPos(content.AssignPlayer)
	return content.Must("string", func(p any) bool {
		// 坐标x
		value := p.(string)
		pos, ok := parseRelativePosFunc(value, playerPos.X)
		posX = pos
		return ok
	}).Must("string", func(p any) bool {
		// 坐标y
		value := p.(string)
		pos, ok := parseRelativePosFunc(value, playerPos.Y)
		posY = pos
		return ok
	}).Must("string", func(p any) bool {
		// 坐标z
		value := p.(string)
		pos, ok := parseRelativePosFunc(value, playerPos.Z)
		posZ = pos
		return ok
	}).Execute(func() bool {
		// 传送玩家至指定的位置
		c.gmCmd.GMTeleportPlayer(content.AssignPlayer.PlayerId, sceneId, posX, posY, posZ)
		// 发送消息给执行者
		content.SendSuccMessage(content.Executor, "已传送至指定位置，指定UID：%v，场景ID：%v，X：%.2f，Y：%.2f，Z：%.2f。", content.AssignPlayer.PlayerId, content.AssignPlayer.GetSceneId(), posX, posY, posZ)
		return true
	})
}

// 传送场景命令

func (c *CommandManager) NewJumpCommandController() *CommandController {
	return &CommandController{
		Name:        "传送场景",
		AliasList:   []string{"jump"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>传送到至指定场景</color>",
		UsageList: []string{
			"{alias} <场景ID> 传送至指定场景",
		},
		Perm: CommandPermNormal,
		Func: c.JumpCommand,
	}
}

func (c *CommandManager) JumpCommand(content *CommandContent) bool {
	var sceneId uint32 // 场景id

	return content.Must("uint32", func(p any) bool {
		sceneId = p.(uint32)
		return true
	}).Execute(func() bool {
		var posX float64
		var posY float64
		var posZ float64
		// 读取场景初始位置
		sceneLuaConfig := gdconf.GetSceneLuaConfigById(int32(sceneId))
		if sceneLuaConfig != nil {
			bornPos := sceneLuaConfig.SceneConfig.BornPos
			posX = float64(bornPos.X)
			posY = float64(bornPos.Y)
			posZ = float64(bornPos.Z)
		} else {
			logger.Error("get scene lua config is nil, sceneId: %v, uid: %v", sceneId, content.AssignPlayer.PlayerId)
		}
		// 传送玩家至指定的位置
		c.gmCmd.GMTeleportPlayer(content.AssignPlayer.PlayerId, sceneId, posX, posY, posZ)
		// 发送消息给执行者
		content.SendSuccMessage(content.Executor, "已传送至指定场景，指定UID：%v，场景ID：%v，X：%.2f，Y：%.2f，Z：%.2f。", content.AssignPlayer.PlayerId, content.AssignPlayer.GetSceneId(), posX, posY, posZ)
		return true
	})
}

// 管理角色命令

func (c *CommandManager) NewAvatarCommandController() *CommandController {
	return &CommandController{
		Name:        "角色",
		AliasList:   []string{"avatar"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>角色</color>",
		UsageList: []string{
			"{alias} <add/del> <角色ID/all>",
		},
		Perm: CommandPermNormal,
		Func: c.AvatarCommand,
	}
}

func (c *CommandManager) AvatarCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "add":
			// 添加角色
			// 判断是否要添加全部角色
			if param == "all" {
				c.gmCmd.GMAddAllAvatar(content.AssignPlayer.PlayerId, 1, 0)
				content.SendSuccMessage(content.Executor, "已添加所有角色，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			// 角色id
			avatarId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMAddAvatar(content.AssignPlayer.PlayerId, uint32(avatarId), 1, 0)
			content.SendSuccMessage(content.Executor, "已添加角色，指定UID：%v，角色ID：%v。", content.AssignPlayer.PlayerId, avatarId)
		case "del":
			// 删除角色
			// 判断是否要删除全部角色
			if param == "all" {
				c.gmCmd.GMDelAllAvatar(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已删除所有角色，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			// 角色id
			avatarId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMDelAvatar(content.AssignPlayer.PlayerId, uint32(avatarId))
			content.SendSuccMessage(content.Executor, "已删除角色，指定UID：%v，角色ID：%v。", content.AssignPlayer.PlayerId, avatarId)
		default:
			return false
		}
		return true
	})
}

// 管理装备命令

func (c *CommandManager) NewEquipCommandController() *CommandController {
	return &CommandController{
		Name:        "装备",
		AliasList:   []string{"equip"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>装备</color>",
		UsageList: []string{
			"{alias} add all 添加全部武器圣遗物",
			"{alias} add <武器ID> [等级] [突破] [精炼] 添加武器",
			"{alias} add <圣遗物ID> [主属性ID] [<副属性ID> <副属性追加次数> ...] 添加圣遗物",
			"{alias} clear weapon 清除全部武器",
			"{alias} clear reliquary 清除全部圣遗物",
		},
		Perm: CommandPermNormal,
		Func: c.EquipCommand,
	}
}

func (c *CommandManager) EquipCommand(content *CommandContent) bool {
	var mode string        // 模式
	var param string       // 参数
	var paramList []uint32 // 参数列表

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Array("uint32", func(p any) bool {
		paramAnyList := p.([]any)
		for _, paramAny := range paramAnyList {
			paramList = append(paramList, paramAny.(uint32))
		}
		return true
	}).Execute(func() bool {
		switch mode {
		case "add":
			if param == "all" {
				c.gmCmd.GMAddAllWeapon(content.AssignPlayer.PlayerId, 1, 1, 0, 0)
				c.gmCmd.GMAddAllReliquary(content.AssignPlayer.PlayerId, 1)
				content.SendSuccMessage(content.Executor, "已添加所有武器圣遗物，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			itemId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			itemDataConfig := gdconf.GetItemDataById(int32(itemId))
			if itemDataConfig == nil {
				return false
			}
			switch itemDataConfig.Type {
			case constant.ITEM_TYPE_WEAPON:
				level := uint8(1)
				promote := uint8(0)
				refinement := uint8(0)
				if len(paramList) > 0 {
					level = uint8(paramList[0])
				}
				if len(paramList) > 1 {
					promote = uint8(paramList[1])
				}
				if len(paramList) > 2 {
					refinement = uint8(paramList[2])
				}
				c.gmCmd.GMAddWeapon(content.AssignPlayer.PlayerId, uint32(itemId), 1, level, promote, refinement)
				content.SendSuccMessage(content.Executor, "已添加武器，指定UID：%v，武器ID：%v，等级：%v，突破：%v，精炼：%v。", content.AssignPlayer.PlayerId, itemId, level, promote, refinement)
			case constant.ITEM_TYPE_RELIQUARY:
				mainPropId := uint32(0)
				appendPropIdList := make([]uint32, 0)
				if len(paramList) > 0 {
					mainPropId = paramList[0]
				}
				if (len(paramList)-1)%2 == 0 {
					newParamList := paramList[1:]
					for i := 0; i < len(newParamList); i += 2 {
						appendPropId := newParamList[i]
						appendCount := newParamList[i+1]
						if appendCount > 100 {
							continue
						}
						for j := 0; j < int(appendCount); j++ {
							appendPropIdList = append(appendPropIdList, appendPropId)
						}
					}
				}
				if len(appendPropIdList) > 1000 {
					appendPropIdList = make([]uint32, 0)
				}
				c.gmCmd.GMAddReliquary(content.AssignPlayer.PlayerId, uint32(itemId), 1, mainPropId, appendPropIdList)
				content.SendSuccMessage(content.Executor, "已添加圣遗物，指定UID：%v，圣遗物ID：%v。", content.AssignPlayer.PlayerId, itemId)
			default:
				return false
			}
		case "clear":
			switch param {
			case "weapon":
				c.gmCmd.GMClearWeapon(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已清除全部武器，指定UID：%v。", content.AssignPlayer.PlayerId)
			case "reliquary":
				c.gmCmd.GMClearReliquary(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已清除全部圣遗物，指定UID：%v。", content.AssignPlayer.PlayerId)
			default:
				return false
			}
		default:
			return false
		}
		return true
	})
}

// 管理道具命令

func (c *CommandManager) NewItemCommandController() *CommandController {
	return &CommandController{
		Name:        "道具",
		AliasList:   []string{"item"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>道具</color>",
		UsageList: []string{
			"{alias} add <道具ID/all> [数量] 添加道具",
			"{alias} clear <道具ID/all> [数量] 清除道具",
		},
		Perm: CommandPermNormal,
		Func: c.ItemCommand,
	}
}

func (c *CommandManager) ItemCommand(content *CommandContent) bool {
	var mode string      // 模式
	var param string     // 参数
	var count uint32 = 1 // 数量

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Option("uint32", func(p any) bool {
		count = p.(uint32)
		return true
	}).Execute(func() bool {
		switch mode {
		case "add":
			if param == "all" {
				c.gmCmd.GMAddAllItem(content.AssignPlayer.PlayerId, count)
				content.SendSuccMessage(content.Executor, "已添加所有道具，指定UID：%v，数量：%v。", content.AssignPlayer.PlayerId, count)
				return true
			}
			itemId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			itemDataConfig := gdconf.GetItemDataById(int32(itemId))
			if itemDataConfig == nil {
				return false
			}
			c.gmCmd.GMAddItem(content.AssignPlayer.PlayerId, uint32(itemId), count)
			content.SendSuccMessage(content.Executor, "已添加道具，指定UID：%v，道具ID：%v，数量：%v。", content.AssignPlayer.PlayerId, itemId, count)
		case "clear":
			if param == "all" {
				c.gmCmd.GMClearItem(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已清除全部道具，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			itemId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMCostItem(content.AssignPlayer.PlayerId, uint32(itemId), count)
			content.SendSuccMessage(content.Executor, "已清除道具，指定UID：%v，道具ID：%v，数量：%v。", content.AssignPlayer.PlayerId, itemId, count)
		default:
			return false
		}
		return true
	})
}

// 杀死实体命令

func (c *CommandManager) NewKillCommandController() *CommandController {
	return &CommandController{
		Name:        "杀死实体",
		AliasList:   []string{"kill"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>杀死实体</color>",
		UsageList: []string{
			"{alias} self 杀死自己",
			"{alias} monster <实体ID/all> 杀死怪物",
		},
		Perm: CommandPermNormal,
		Func: c.KillCommand,
	}
}

func (c *CommandManager) KillCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Option("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "self":
			// 杀死自己
			c.gmCmd.GMKillSelf(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已杀死自己，指定UID：%v。", content.AssignPlayer.PlayerId)
		case "monster":
			// 杀死怪物
			switch param {
			case "":
				// 怪物的话必须指定目标
				content.SetElse(func() {
					content.SendFailMessage(content.Executor, "参数不足，必须指定杀死的怪物。")
				})
				return false
			case "all":
				// 目标为全部怪物
				c.gmCmd.GMKillAllMonster(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已杀死所有怪物，指定UID：%v。", content.AssignPlayer.PlayerId)
			default:
				// 实体id
				entityId, err := strconv.ParseUint(param, 10, 32)
				if err != nil {
					return false
				}
				c.gmCmd.GMKillMonster(content.AssignPlayer.PlayerId, uint32(entityId))
				content.SendSuccMessage(content.Executor, "已杀死目标怪物，指定UID：%v，实体ID：%v。", content.AssignPlayer.PlayerId, entityId)
			}
		default:
			return false
		}
		return true
	})
}

// 生成怪物命令

func (c *CommandManager) NewMonsterCommandController() *CommandController {
	return &CommandController{
		Name:        "怪物",
		AliasList:   []string{"monster"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>怪物</color>",
		UsageList: []string{
			"{alias} <怪物ID> [数量] [等级] [姿势] [坐标X] [坐标Y] [坐标Z] 生成怪物",
		},
		Perm: CommandPermNormal,
		Func: c.MonsterCommand,
	}
}

func (c *CommandManager) MonsterCommand(content *CommandContent) bool {
	var monsterId uint32 // 怪物id
	var count uint32 = 1 // 数量
	var level uint8 = 1  // 等级
	var pose uint32      // 姿势
	pos := GAME.GetPlayerPos(content.AssignPlayer)
	var posX = pos.X // 坐标x
	var posY = pos.Y // 坐标y
	var posZ = pos.Z // 坐标z

	return content.Must("uint32", func(p any) bool {
		monsterId = p.(uint32)
		return true
	}).Option("uint32", func(p any) bool {
		count = p.(uint32)
		return true
	}).Option("uint8", func(p any) bool {
		level = p.(uint8)
		return true
	}).Option("uint32", func(p any) bool {
		pose = p.(uint32)
		return true
	}).Option("float64", func(p any) bool {
		posX = p.(float64)
		return true
	}).Option("float64", func(p any) bool {
		posY = p.(float64)
		return true
	}).Option("float64", func(p any) bool {
		posZ = p.(float64)
		return true
	}).Execute(func() bool {
		_ = pose
		c.gmCmd.GMCreateMonster(content.AssignPlayer.PlayerId, monsterId, posX, posY, posZ, count, level)
		return true
	})
}

// 生成物件命令

func (c *CommandManager) NewGadgetCommandController() *CommandController {
	return &CommandController{
		Name:        "物件",
		AliasList:   []string{"gadget"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>物件</color>",
		UsageList: []string{
			"{alias} <物件ID> [数量] 附近生成物件",
		},
		Perm: CommandPermNormal,
		Func: c.GadgetCommand,
	}
}

func (c *CommandManager) GadgetCommand(content *CommandContent) bool {
	var gadgetId uint32  // 物件id
	var count uint32 = 1 // 数量

	return content.Must("uint32", func(p any) bool {
		gadgetId = p.(uint32)
		return true
	}).Option("uint32", func(p any) bool {
		count = p.(uint32)
		return true
	}).Execute(func() bool {
		c.gmCmd.GMCreateGadget(content.AssignPlayer.PlayerId, gadgetId, count)
		return true
	})
}

// 管理任务命令

func (c *CommandManager) NewQuestCommandController() *CommandController {
	return &CommandController{
		Name:        "任务",
		AliasList:   []string{"quest"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>任务</color>",
		UsageList: []string{
			"{alias} <add/accept> <任务ID> 接受任务",
			"{alias} finish <任务ID/all> 完成任务",
			"{alias} clear all 清除全部任务",
		},
		Perm: CommandPermNormal,
		Func: c.QuestCommand,
	}
}

func (c *CommandManager) QuestCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "add", "accept":
			// 任务id
			questId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			// 添加指定任务
			// 接受指定任务 暂时与添加相同
			c.gmCmd.GMAddQuest(content.AssignPlayer.PlayerId, uint32(questId))
			content.SendSuccMessage(content.Executor, "已添加任务，指定UID：%v，任务ID：%v。", content.AssignPlayer.PlayerId, questId)
		case "finish":
			// 完成指定任务
			if param == "all" {
				// 强制完成当前所有任务
				c.gmCmd.GMForceFinishAllQuest(content.AssignPlayer.PlayerId)
				content.SendSuccMessage(content.Executor, "已完成当前全部任务，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			// 任务id
			questId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMFinishQuest(content.AssignPlayer.PlayerId, uint32(questId))
			content.SendSuccMessage(content.Executor, "已完成玩家任务，指定UID：%v，任务ID：%v。", content.AssignPlayer.PlayerId, questId)
		case "clear":
			c.gmCmd.GMClearQuest(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已清除全部任务，指定UID：%v。", content.AssignPlayer.PlayerId)
		default:
			return false
		}
		return true
	})
}

// 解锁锚点命令

func (c *CommandManager) NewPointCommandController() *CommandController {
	return &CommandController{
		Name:        "锚点",
		AliasList:   []string{"point"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>锚点</color>",
		UsageList: []string{
			"{alias} [场景ID] <锚点ID/all> 解锁锚点",
		},
		Perm: CommandPermNormal,
		Func: c.PointCommand,
	}
}

func (c *CommandManager) PointCommand(content *CommandContent) bool {
	var sceneId = content.AssignPlayer.GetSceneId() // 场景id
	var param string                                // 参数

	return content.Option("uint32", func(p any) bool {
		sceneId = p.(uint32)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		if param == "all" {
			// 解锁当前场景所有锚点
			c.gmCmd.GMUnlockAllPoint(content.AssignPlayer.PlayerId, sceneId)
			content.SendSuccMessage(content.Executor, "已解锁所有锚点，指定UID：%v，场景ID：%v。", content.AssignPlayer.PlayerId, sceneId)
			return true
		}
		// 锚点id
		pointId, err := strconv.ParseUint(param, 10, 32)
		if err != nil {
			return false
		}
		c.gmCmd.GMUnlockPoint(content.AssignPlayer.PlayerId, sceneId, uint32(pointId))
		content.SendSuccMessage(content.Executor, "已解锁锚点，指定UID：%v，场景ID：%v，锚点ID：%v。", content.AssignPlayer.PlayerId, sceneId, pointId)
		return true
	})
}

// 解锁区域命令

func (c *CommandManager) NewAreaCommandController() *CommandController {
	return &CommandController{
		Name:        "区域",
		AliasList:   []string{"area"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>区域</color>",
		UsageList: []string{
			"{alias} [场景ID] <区域ID/all> 解锁区域",
		},
		Perm: CommandPermNormal,
		Func: c.AreaCommand,
	}
}

func (c *CommandManager) AreaCommand(content *CommandContent) bool {
	var sceneId = content.AssignPlayer.GetSceneId() // 场景id
	var param string                                // 参数

	return content.Option("uint32", func(p any) bool {
		sceneId = p.(uint32)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		if param == "all" {
			// 解锁当前场景所有区域
			c.gmCmd.GMUnlockAllArea(content.AssignPlayer.PlayerId, sceneId)
			content.SendSuccMessage(content.Executor, "已解锁所有区域，指定UID：%v，场景ID：%v。", content.AssignPlayer.PlayerId, sceneId)
			return true
		}
		// 区域id
		areaId, err := strconv.ParseUint(param, 10, 32)
		if err != nil {
			return false
		}
		c.gmCmd.GMUnlockArea(content.AssignPlayer.PlayerId, sceneId, uint32(areaId))
		content.SendSuccMessage(content.Executor, "已解锁区域，指定UID：%v，场景ID：%v，区域ID：%v。", content.AssignPlayer.PlayerId, sceneId, areaId)
		return true
	})
}

// 更改天气命令

func (c *CommandManager) NewWeatherCommandController() *CommandController {
	return &CommandController{
		Name:        "天气",
		AliasList:   []string{"weather"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>天气</color>",
		UsageList: []string{
			"{alias} <气象类型> 更改天气",
		},
		Perm: CommandPermNormal,
		Func: c.WeatherCommand,
	}
}

func (c *CommandManager) WeatherCommand(content *CommandContent) bool {
	var climateType uint32 // 气象类型

	return content.Must("uint32", func(p any) bool {
		climateType = p.(uint32)
		return true
	}).Execute(func() bool {
		// 设置天气
		c.gmCmd.GMSetWeather(content.AssignPlayer.PlayerId, climateType)
		content.SendSuccMessage(content.Executor, "已更改天气，指定UID：%v，气象类型：%v。", content.AssignPlayer.PlayerId, climateType)
		return true
	})
}

// 功能开放命令

func (c *CommandManager) NewOpenStateCommandController() *CommandController {
	return &CommandController{
		Name:        "功能开放",
		AliasList:   []string{"openstate"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>功能开放</color>",
		UsageList: []string{
			"{alias} <功能ID/all> <1/0> 设置功能开放值",
		},
		Perm: CommandPermNormal,
		Func: c.OpenStateCommand,
	}
}

func (c *CommandManager) OpenStateCommand(content *CommandContent) bool {
	var param1 string // 参数1
	var param2 int    // 参数2

	return content.Must("string", func(p any) bool {
		param1 = p.(string)
		return true
	}).Must("int", func(p any) bool {
		param2 = p.(int)
		return true
	}).Execute(func() bool {
		if param1 == "all" {
			c.gmCmd.GMSetAllOpenState(content.AssignPlayer.PlayerId, uint32(param2))
			content.SendSuccMessage(content.Executor, "已设置全部功能开放值，指定UID：%v，值：%v。", content.AssignPlayer.PlayerId, param2)
			return true
		}
		openStateId, err := strconv.ParseUint(param1, 10, 32)
		if err != nil {
			return false
		}
		c.gmCmd.GMSetOpenState(content.AssignPlayer.PlayerId, uint32(openStateId), uint32(param2))
		content.SendSuccMessage(content.Executor, "已设置功能开放值，指定UID：%v，功能开放ID：%v，值：%v。", content.AssignPlayer.PlayerId, openStateId, param2)
		return true
	})
}

// 命座命令

func (c *CommandManager) NewTalentCommandController() *CommandController {
	return &CommandController{
		Name:        "命座",
		AliasList:   []string{"talent"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>命座</color>",
		UsageList: []string{
			"{alias} <unlock/lock> <命座ID/all> 解锁或锁定当前角色命座",
		},
		Perm: CommandPermNormal,
		Func: c.TalentCommand,
	}
}

func (c *CommandManager) TalentCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "unlock":
			if param == "all" {
				c.gmCmd.GMSetTalentUnlock(content.AssignPlayer.PlayerId, 0, true)
				content.SendSuccMessage(content.Executor, "已解锁当前角色命座，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			talentId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMSetTalentUnlock(content.AssignPlayer.PlayerId, uint32(talentId), true)
			content.SendSuccMessage(content.Executor, "已解锁当前角色命座，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "lock":
			if param == "all" {
				c.gmCmd.GMSetTalentUnlock(content.AssignPlayer.PlayerId, 0, false)
				content.SendSuccMessage(content.Executor, "已锁定当前角色命座，指定UID：%v。", content.AssignPlayer.PlayerId)
				return true
			}
			talentId, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMSetTalentUnlock(content.AssignPlayer.PlayerId, uint32(talentId), false)
			content.SendSuccMessage(content.Executor, "已锁定当前角色命座，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		default:
			return false
		}
	})
}

// 玩家等级命令

func (c *CommandManager) NewPlayerCommandController() *CommandController {
	return &CommandController{
		Name:        "玩家等级",
		AliasList:   []string{"player"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>玩家等级</color>",
		UsageList: []string{
			"{alias} level <冒险等级> 设置冒险等级",
		},
		Perm: CommandPermNormal,
		Func: c.PlayerCommand,
	}
}

func (c *CommandManager) PlayerCommand(content *CommandContent) bool {
	var mode string  // 模式
	var param string // 参数

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "level":
			level, err := strconv.ParseUint(param, 10, 32)
			if err != nil {
				return false
			}
			c.gmCmd.GMSetPlayerLevelExp(content.AssignPlayer.PlayerId, uint32(level), 0)
			content.SendSuccMessage(content.Executor, "已设置冒险等级，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		default:
			return false
		}
	})
}

// 角色等级命令

func (c *CommandManager) NewLevelCommandController() *CommandController {
	return &CommandController{
		Name:        "角色等级",
		AliasList:   []string{"level"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>角色等级</color>",
		UsageList: []string{
			"{alias} <等级> 设置当前角色等级",
		},
		Perm: CommandPermNormal,
		Func: c.LevelCommand,
	}
}

func (c *CommandManager) LevelCommand(content *CommandContent) bool {
	var param string // 参数

	return content.Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		level, err := strconv.ParseUint(param, 10, 32)
		if err != nil {
			return false
		}
		c.gmCmd.GMSetPlayerAvatarLevelExp(content.AssignPlayer.PlayerId, uint8(level), 0)
		content.SendSuccMessage(content.Executor, "已设置当前角色等级，指定UID：%v。", content.AssignPlayer.PlayerId)
		return true
	})
}

// 角色突破命令

func (c *CommandManager) NewBreakCommandController() *CommandController {
	return &CommandController{
		Name:        "角色突破",
		AliasList:   []string{"break"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>角色突破</color>",
		UsageList: []string{
			"{alias} <突破阶段> 设置角色突破阶段",
		},
		Perm: CommandPermNormal,
		Func: c.BreakCommand,
	}
}

func (c *CommandManager) BreakCommand(content *CommandContent) bool {
	var param string // 参数

	return content.Must("string", func(p any) bool {
		param = p.(string)
		return true
	}).Execute(func() bool {
		promote, err := strconv.ParseUint(param, 10, 32)
		if err != nil {
			return false
		}
		c.gmCmd.GMSetPlayerAvatarPromote(content.AssignPlayer.PlayerId, uint8(promote))
		content.SendSuccMessage(content.Executor, "已设置当前角色突破阶段，指定UID：%v。", content.AssignPlayer.PlayerId)
		return true
	})
}

// 清除命令

func (c *CommandManager) NewClearCommandController() *CommandController {
	return &CommandController{
		Name:        "清除",
		AliasList:   []string{"clear"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>清除</color>",
		UsageList: []string{
			"{alias} all 清除玩家数据",
		},
		Perm: CommandPermNormal,
		Func: c.ClearCommand,
	}
}

func (c *CommandManager) ClearCommand(content *CommandContent) bool {
	var mode string // 模式

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "all":
			c.gmCmd.GMClearPlayer(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已清除玩家数据，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		default:
			return false
		}
	})
}

// 调试命令

func (c *CommandManager) NewDebugCommandController() *CommandController {
	return &CommandController{
		Name:        "调试",
		AliasList:   []string{"debug"},
		Description: "<color=#FFFFCC>{alias}</color> <color=#FFCC99>调试</color>",
		UsageList: []string{
			"{alias} freemode 自由探索模式",
			"{alias} clearworld 清除大世界数据",
			"{alias} scenetag 添加全部场景标签",
			"{alias} notsave 本次离线回档",
			"{alias} xluaswitch 开关xLua",
			"{alias} gcgtest 七圣召唤测试",
		},
		Perm: CommandPermNormal,
		Func: c.DebugCommand,
	}
}

func (c *CommandManager) DebugCommand(content *CommandContent) bool {
	var mode string // 模式

	return content.Must("string", func(p any) bool {
		mode = p.(string)
		return true
	}).Execute(func() bool {
		switch mode {
		case "freemode":
			c.gmCmd.GMFreeMode(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已开启自由探索模式，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "clearworld":
			c.gmCmd.GMClearWorld(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已清除大世界数据，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "scenetag":
			c.gmCmd.GMAddAllSceneTag(content.AssignPlayer.PlayerId, content.AssignPlayer.GetSceneId())
			content.SendSuccMessage(content.Executor, "已添加全部场景标签，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "notsave":
			c.gmCmd.GMNotSave(content.AssignPlayer.PlayerId)
			content.SendSuccMessage(content.Executor, "已设置本次离线回档，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		case "xluaswitch":
			if !content.AssignPlayer.XLuaDebug {
				content.AssignPlayer.XLuaDebug = true
				content.SendSuccMessage(content.Executor, "已开启客户端XLUA调试，指定UID：%v。", content.AssignPlayer.PlayerId)
			} else {
				content.AssignPlayer.XLuaDebug = false
				content.SendSuccMessage(content.Executor, "已关闭客户端XLUA调试，指定UID：%v。", content.AssignPlayer.PlayerId)
			}
			return true
		case "gcgtest":
			GAME.GCGStartChallenge(content.AssignPlayer)
			content.SendSuccMessage(content.Executor, "已开始七圣召唤对局，指定UID：%v。", content.AssignPlayer.PlayerId)
			return true
		default:
			return false
		}
	})
}
