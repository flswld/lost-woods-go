-- 原神KCP协议
local hk4e_kcp_protocol = Proto("HK4E_KCP", "Genshin Impact KCP Protocol")

-- 协议字段
local sess = ProtoField.uint32("hk4e_kcp.sess", "sess", base.DEC)
local conv = ProtoField.uint32("hk4e_kcp.conv", "conv", base.DEC)
local cmd = ProtoField.uint8("hk4e_kcp.cmd", "cmd", base.DEC)
local frg = ProtoField.uint8("hk4e_kcp.frg", "frg", base.DEC)
local wnd = ProtoField.uint16("hk4e_kcp.wnd", "wnd", base.DEC)
local ts = ProtoField.uint32("hk4e_kcp.ts", "ts", base.DEC)
local sn = ProtoField.uint32("hk4e_kcp.sn", "sn", base.DEC)
local una = ProtoField.uint32("hk4e_kcp.una", "una", base.DEC)
local len = ProtoField.uint32("hk4e_kcp.len", "len", base.DEC)
local data = ProtoField.bytes("hk4e_kcp.data", "data", base.NONE)
local byte_check = ProtoField.uint32("hk4e_kcp.byte_check", "byte_check", base.DEC)
local enet_sess = ProtoField.uint32("hk4e_kcp.enet_sess", "enet_sess", base.DEC)
local enet_conv = ProtoField.uint32("hk4e_kcp.enet_conv", "enet_conv", base.DEC)
local enet_type = ProtoField.uint32("hk4e_kcp.enet_type", "enet_type", base.DEC)
local enet_state = ProtoField.string("hk4e_kcp.enet_state", "enet_state")

hk4e_kcp_protocol.fields = { sess, conv, cmd, frg, wnd, ts, sn, una, len, byte_check, enet_sess, enet_conv, enet_type, enet_state, data }

local KCP_HEADER_LEN = 28
local BYTE_CHECK_LEN = 4

local ENET_SYN_HEAD = 0x000000ff
local ENET_SYN_TAIL = 0xffffffff
local ENET_EST_HEAD = 0x00000145
local ENET_EST_TAIL = 0x14514545
local ENET_FIN_HEAD = 0x00000194
local ENET_FIN_TAIL = 0x19419494
local ENET_PING_HEAD = 0x00000227
local ENET_PING_TAIL = 0x22722727
local ENET_CLIENT_CONNECT_KEY = 1234567890

local ENET_TYPE_NAMES = {
    [0] = "EnetTimeout",
    [1] = "EnetClientClose",
    [2] = "EnetClientRebindFail",
    [3] = "EnetClientShutdown",
    [4] = "EnetServerRelogin",
    [5] = "EnetServerKick",
    [6] = "EnetServerShutdown",
    [7] = "EnetNotFoundSession",
    [8] = "EnetLoginUnfinished",
    [9] = "EnetPacketFreqTooHigh",
    [10] = "EnetPingTimeout",
    [11] = "EnetTransferFailed",
    [12] = "EnetServerKillClient",
    [13] = "EnetCheckMoveSpeed",
    [14] = "EnetAccountPasswordChange",
    [15] = "EnetSecurityKick",
    [16] = "EnetLuaShellTimeout",
    [17] = "EnetSDKFailKick",
    [18] = "EnetPacketCostTime",
    [19] = "EnetPacketUnionFreq",
    [20] = "EnetWaitSndMax",
    [987654321] = "EnetClientEditorConnectKey",
    [1234567890] = "EnetClientConnectKey",
}

local function is_kcp_cmd(cmd_val)
    return cmd_val == 81 or cmd_val == 82 or cmd_val == 83 or cmd_val == 84
end

local function get_cmd_name(cmd_val)
    if cmd_val == 81 then
        return "PSH"
    elseif cmd_val == 82 then
        return "ACK"
    elseif cmd_val == 83 then
        return "ASK"
    elseif cmd_val == 84 then
        return "TELL"
    end
    return string.format("UNKNOWN(%u)", cmd_val)
end

local function count_kcp_segments(buffer, byte_check_enable)
    local packet_len = buffer:len()
    local header_len = KCP_HEADER_LEN
    if byte_check_enable then
        header_len = header_len + BYTE_CHECK_LEN
    end

    local offset = 0
    local segment_count = 0
    while offset < packet_len do
        if packet_len - offset < header_len then
            return nil
        end

        local cmd_val = buffer(offset + 8, 1):le_uint()
        if not is_kcp_cmd(cmd_val) then
            return nil
        end

        local data_len = buffer(offset + 24, 4):le_uint()
        local next_offset = offset + header_len + data_len
        if next_offset > packet_len then
            return nil
        end

        segment_count = segment_count + 1
        offset = next_offset
    end

    return segment_count
end

local function detect_byte_check(buffer)
    local segment_count = count_kcp_segments(buffer, true)
    if segment_count ~= nil then
        return true, segment_count
    end

    segment_count = count_kcp_segments(buffer, false)
    if segment_count ~= nil then
        return false, segment_count
    end

    return nil, 0
end

local function get_enet_state(buffer)
    local head = buffer(0, 4):uint()
    local tail = buffer(16, 4):uint()

    if head == ENET_SYN_HEAD and tail == ENET_SYN_TAIL then
        return "SYN"
    elseif head == ENET_EST_HEAD and tail == ENET_EST_TAIL then
        return "EST"
    elseif head == ENET_FIN_HEAD and tail == ENET_FIN_TAIL then
        return "FIN"
    elseif head == ENET_PING_HEAD and tail == ENET_PING_TAIL then
        return "PING"
    end

    return nil
end

local function get_enet_type_name(type_val)
    return ENET_TYPE_NAMES[type_val] or string.format("UNKNOWN(%u)", type_val)
end

local function get_enet_type_text(state_name, type_val)
    if state_name == "PING" then
        return string.format("PingOffsetMs=%d", type_val - ENET_CLIENT_CONNECT_KEY)
    end
    return get_enet_type_name(type_val)
end

-- 解析器
function hk4e_kcp_protocol.dissector(buffer, pinfo, tree)
    local length = buffer:len()
    if length == 0 then
        return
    end

    pinfo.cols.protocol = hk4e_kcp_protocol.name

    if length == 20 then
        local enet_state_name = get_enet_state(buffer)
        if enet_state_name ~= nil then
            local enet_sess_val = buffer(4, 4):uint()
            local enet_conv_val = buffer(8, 4):uint()
            local enet_type_val = buffer(12, 4):uint()
            local enet_type_text = get_enet_type_text(enet_state_name, enet_type_val)
            pinfo.cols.info:append(string.format(" [ENET %s] Sess=%u Conv=%u Type=%u(%s)", enet_state_name, enet_sess_val, enet_conv_val, enet_type_val, enet_type_text))

            local subtree = tree:add(hk4e_kcp_protocol, buffer(), "Genshin Impact KCP ENET Protocol")
            subtree:add(enet_sess, buffer(4, 4))
            subtree:add(enet_conv, buffer(8, 4))
            subtree:add(enet_type, buffer(12, 4)):append_text(" (" .. enet_type_text .. ")")
            subtree:add(enet_state, buffer(0, 20), enet_state_name)
            return
        end
    end

    local byte_check_enable, segment_count = detect_byte_check(buffer)
    if byte_check_enable == nil then
        pinfo.cols.info:append(" [Malformed KCP]")
        return
    end

    local header_len = KCP_HEADER_LEN
    if byte_check_enable then
        header_len = header_len + BYTE_CHECK_LEN
    end

    local first_cmd_name = get_cmd_name(buffer(8, 1):le_uint())
    local first_sn = buffer(16, 4):le_uint()
    local first_una = buffer(20, 4):le_uint()
    local first_len = buffer(24, 4):le_uint()
    local info_text = string.format(" [%s] Sn=%u Una=%u DataLen=%u", first_cmd_name, first_sn, first_una, first_len)
    if segment_count > 1 then
        info_text = info_text .. string.format(" Segs=%u", segment_count)
    end
    pinfo.cols.info:append(info_text)

    -- 解析多个KCP包
    local offset = 0
    while offset < buffer:len() do
        local sess_buf = buffer(offset + 0, 4)
        local conv_buf = buffer(offset + 4, 4)
        local cmd_buf = buffer(offset + 8, 1)
        local wnd_buf = buffer(offset + 10, 2)
        local sn_buf = buffer(offset + 16, 4)
        local len_buf = buffer(offset + 24, 4)

        local cmd_name = get_cmd_name(cmd_buf:le_uint())
        local data_len = len_buf:le_uint()
        local segment_len = header_len + data_len

        local subtree = tree:add(hk4e_kcp_protocol, buffer(offset, segment_len), "Genshin Impact KCP Protocol")
        subtree:add_le(sess, sess_buf)
        subtree:add_le(conv, conv_buf)
        subtree:add_le(cmd, cmd_buf):append_text(" (" .. cmd_name .. ")")
        subtree:add_le(frg, buffer(offset + 9, 1))
        subtree:add_le(wnd, wnd_buf)
        subtree:add_le(ts, buffer(offset + 12, 4))
        subtree:add_le(sn, sn_buf)
        subtree:add_le(una, buffer(offset + 20, 4))
        subtree:add_le(len, len_buf)
        if byte_check_enable then
            subtree:add_le(byte_check, buffer(offset + 28, BYTE_CHECK_LEN))
        end

        if data_len ~= 0 then
            local data_buf = buffer(offset + header_len, data_len)
            subtree:add(data, data_buf)
        end

        offset = offset + segment_len
    end
end

-- 注册到UDP常用KCP端口
local udp_port = DissectorTable.get("udp.port")
udp_port:add(22222, hk4e_kcp_protocol)
udp_port:add(22101, hk4e_kcp_protocol)
udp_port:add(22102, hk4e_kcp_protocol)
