-- The whole proposed design: the node acts on mpv only through this script, and hears mpv only
-- from it. Every message is a snapshot taken by the script at the moment it says, with its cause:
--   wp <kind> <id> <pause> <time-pos> <playlist-pos> <paused-for-cache>
-- kinds: hello (id = this script's name), ack (our pause applied), pause (pause marked changed),
-- began (a seek event; id = ours if it carries ours), landed (a seek ended; id = ours if it carried ours), ready (a restart with no seek: load, cache),
-- loaded (file-loaded; id = ours, "eof" for mpv's own move at the end, "" for the person's),
-- cache (paused-for-cache marked changed), end (end-file; id = its reason), held (a seek waiting
-- on our move's load), refused (a command mpv refused).

local function snap(kind, id)
    mp.commandv("script-message", "wp", kind, id or "", mp.get_property("pause", ""),
        mp.get_property("time-pos", ""), mp.get_property("playlist-pos", ""),
        mp.get_property("paused-for-cache", ""))
end

mp.register_script_message("wp-hello", function() snap("hello", mp.get_script_name()) end)

mp.register_script_message("wp-pause", function(v, id)
    mp.set_property("pause", v)
    snap("ack", id)
end)

-- Seeks: run async, so the reply sits in this script's event queue in order with mpv's seek
-- events: the first seek event after it is the seek that carries ours.
local replied, carrying, seeking = {}, nil, false
local loading, held = false, nil

local function seek(pos, id)
    mp.command_native_async({"seek", pos, "absolute+exact"}, function(ok)
        if ok then table.insert(replied, id) else snap("refused", id) end
    end)
end

mp.register_script_message("wp-seek", function(pos, id)
    if loading then
        held = {pos, id}
        snap("held", id)
        return
    end
    seek(pos, id)
end)

mp.register_event("seek", function()
    seeking = true
    if #replied > 0 then
        carrying = replied[#replied]
        replied = {}
    end
    snap("began", carrying)
end)

mp.register_event("playback-restart", function()
    if seeking then
        snap("landed", carrying)
    else
        snap("ready")
    end
    seeking, carrying = false, nil
end)

-- Moves: the same fence, on start-file. A seek sent while our move loads waits for its file.
local moving, mcarry, ateof = {}, nil, false

mp.register_script_message("wp-move", function(idx, id)
    if mp.get_property("playlist-pos") == idx and not loading then
        snap("ack", id)
        return
    end
    loading = true
    mp.command_native_async({"set", "playlist-pos", idx}, function(ok)
        if ok then table.insert(moving, id) else loading = false; snap("refused", id) end
    end)
end)

mp.register_event("end-file", function(e)
    ateof = e.reason == "eof"
    snap("end", e.reason)
end)

mp.register_event("start-file", function()
    if #moving > 0 then
        mcarry = moving[#moving]
        moving = {}
    elseif ateof then
        mcarry = "eof"
    end
    ateof = false
end)

mp.register_event("file-loaded", function()
    local by = mcarry
    mcarry = nil
    seeking, carrying, replied = false, nil, {}
    if #moving == 0 then
        loading = false
    end
    snap("loaded", by)
    if held and not loading then
        local h = held
        held = nil
        seek(h[1], h[2])
    end
end)

mp.observe_property("pause", "none", function() snap("pause") end)
mp.observe_property("paused-for-cache", "none", function() snap("cache") end)
