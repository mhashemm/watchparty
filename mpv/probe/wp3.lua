-- The one-message design: the node sends the whole state it adopts, the script applies it in
-- one handler (stop, move, seek, play) and acks once mpv has queued all of it. A move waits for
-- its file: the rest is applied at its file-loaded. Every message is a
-- snapshot, in wp2.lua's format:
--   wp <kind> <id> <pause> <time-pos> <playlist-pos> <paused-for-cache>

local function snap(kind, id)
    mp.commandv("script-message", "wp", kind, id or "", mp.get_property("pause", ""),
        mp.get_property("time-pos", ""), mp.get_property("playlist-pos", ""),
        mp.get_property("paused-for-cache", ""))
end

local replied, waiting = {}, nil

-- a seek is made stopped; play, if asked, once it is queued
local function apply(paused, second, id)
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
end

-- wp-go <yes|no> <second or -> <entry or -> <id>: another entry opens stopped
mp.register_script_message("wp-go", function(paused, second, entry, id)
    if entry ~= "-" and mp.get_property("playlist-pos") ~= entry then
        mp.set_property("pause", "yes")
        waiting = {paused, second, id, entry}
        mp.set_property("playlist-pos", entry)
        return
    end
    apply(paused, second, id)
end)

-- the file a move waits for, or another the person or the file's end took mpv to: the ack
-- then says that entry, and the node reads it as theirs
mp.register_event("file-loaded", function()
    local w = waiting
    if not w then return end
    waiting = nil
    if mp.get_property("playlist-pos") == w[4] then
        apply(w[1], w[2], w[3])
    else
        snap("ack", w[3])
    end
end)

mp.register_event("start-file", function() replied = {} end)
mp.register_event("end-file", function(e) snap("end", e.reason) end)
mp.observe_property("paused-for-cache", "none", function() snap("cache") end)

mp.observe_property("pause", "none", function() snap("pause") end)

mp.register_event("seek", function()
    snap("seek", replied[#replied])
    replied = {}
end)
