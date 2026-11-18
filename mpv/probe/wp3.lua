-- The one-message design: the node sends the whole state it adopts, the script applies it in
-- one handler (stop, seek, play) and acks once mpv has queued all of it. Every message is a
-- snapshot, in wp2.lua's format:
--   wp <kind> <id> <pause> <time-pos> <playlist-pos> <paused-for-cache>

local function snap(kind, id)
    mp.commandv("script-message", "wp", kind, id or "", mp.get_property("pause", ""),
        mp.get_property("time-pos", ""), mp.get_property("playlist-pos", ""),
        mp.get_property("paused-for-cache", ""))
end

local replied = {}

-- wp-go <yes|no> <second or -> <id>: a seek is made stopped; play, if asked, once it is queued
mp.register_script_message("wp-go", function(paused, second, id)
    local function finish()
        if paused == "no" then mp.set_property("pause", "no") end
        snap("ack", id)
    end
    if second ~= "-" then
        mp.set_property("pause", "yes")
        mp.command_native_async({"seek", second, "absolute+exact"}, function(ok)
            if ok then table.insert(replied, id) end
            finish()
        end)
        return
    end
    mp.set_property("pause", paused)
    finish()
end)

mp.observe_property("pause", "none", function() snap("pause") end)

mp.register_event("seek", function()
    snap("seek", replied[#replied])
    replied = {}
end)
