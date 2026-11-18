-- The node's own way into mpv (mpv/ORDER.md): mpv runs it in order with everything else it
-- does, and what it says reaches the node as client-message events, which mpv never merges.
-- It decides nothing; the rules are in Go. Every message after its name says what mpv holds
-- when it is sent: watchparty <kind> <id> <pause> <time-pos>.

local function say(kind, id)
    mp.commandv("script-message", "watchparty", kind, id or "", mp.get_property("pause", ""),
        mp.get_property("time-pos", ""))
end

-- hello: the node learns this script's name by asking everyone, as a user's script with the
-- same file name would take the name and leave this one another; and what mpv holds, as the
-- observer below fires once as this script starts, which can be before the node connects
mp.register_script_message("watchparty-hello", function() say("hello", mp.get_script_name()) end)

-- every change mark of pause, also one that ends where it was: mpv reports nothing of a set
-- the person's key undid at once (ORDER.md)
mp.observe_property("pause", "none", function() say("pause") end)

-- go is the whole state the node adopts, in one message, so a person's key lands before it or
-- after it, never between its steps: stopped, on the entry, on the second, then playing if
-- asked. ack: once mpv has all of it, with what mpv holds then. A seek runs async: its reply
-- comes in this script's events in order with mpv's seek events, so the first seek after it
-- is the one that carries the node's. A move waits for its file (heading), and a go meanwhile
-- replaces the one waiting.
local replied, waiting, heading = {}, nil, nil

local function apply(paused, second, id)
    local function finish()
        if paused == "no" then mp.set_property("pause", "no") end
        say("ack", id)
    end
    if second == "-" then
        mp.set_property("pause", paused)
        finish()
        return
    end
    mp.set_property("pause", "yes")
    mp.command_native_async({"seek", second, "absolute+exact"}, function(ok)
        if not ok then
            say("refused", id)
            return
        end
        table.insert(replied, id)
        finish()
    end)
end

mp.register_script_message("watchparty-go", function(paused, second, entry, id)
    if waiting then
        say("ack", waiting.id)
        waiting = nil
    end
    -- a go for the file still loading waits for it too: a seek before it loads never runs
    if entry == "-" and heading then entry = heading end
    local on = heading or mp.get_property("playlist-pos")
    if entry ~= "-" and (heading or on ~= entry) then
        if on ~= entry then
            mp.set_property("pause", "yes")
            heading = entry
            mp.set_property("playlist-pos", entry)
        end
        waiting = {paused = paused, second = second, entry = entry, id = id}
        return
    end
    apply(paused, second, id)
end)

-- the file a move waits for, or another the person's key or a file's end took mpv to, which
-- the node hears from mpv: the ack alone
mp.register_event("file-loaded", function()
    heading = nil
    local w = waiting
    if not w then return end
    waiting = nil
    if mp.get_property("playlist-pos") == w.entry then
        apply(w.paused, w.second, w.id)
    else
        say("ack", w.id)
    end
end)

-- seek: where a seek goes, read at its seek event, which is where it lands (ORDER.md), with
-- the node's id if it carries the node's
mp.register_event("seek", function()
    say("seek", replied[#replied])
    replied = {}
end)

-- a seek queued for the file mpv leaves never runs (MPV.md, "Playlists")
mp.register_event("start-file", function() replied = {} end)
