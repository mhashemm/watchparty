-- The node's own way into mpv (mpv/ORDER.md): mpv runs it in order with everything else it
-- does, and what it says reaches the node as client-message events, which mpv never merges.
-- It decides nothing; the rules are in Go. Every message after its name says what mpv holds
-- when it is sent, and its number:
-- watchparty <kind> <id> <pause> <time-pos> <n> <over|> <playlist-pos>.

-- said: what each recent message said, by its number, so a set can tell what the node had read
local n, said = 0, {}
-- opened: the entry the last start opened
local opened = nil

-- entry: the one a start says it opened, else playlist-pos as mpv holds it now
local function say(kind, id, over, entry)
    n = n + 1
    local paused = mp.get_property("pause", "")
    entry = entry or mp.get_property("playlist-pos", "")
    local person = (id or "") == "" and (kind == "seek" or kind == "start")
    said[n] = {pause = paused, entry = entry, person = person, auto = kind == "start" and id == "auto"}
    said[n - 256] = nil
    local pos = mp.get_property("time-pos", "")
    -- set to another entry that hasn't started: time-pos is still the old file's (0.42 keeps
    -- it as it leaves; ORDER.md), and the new one opens at 0
    if opened and mp.get_property("playlist-pos") ~= opened then pos = "" end
    mp.commandv("script-message", "watchparty", kind, id or "", paused,
        pos, tostring(n), over and "over" or "", entry)
end

-- hello: the node learns this script's name by asking everyone, as a user's script with the
-- same file name would take the name and leave this one another; and what mpv holds, as the
-- observer below fires once as this script starts, which can be before the node connects
mp.register_script_message("watchparty-hello", function() say("hello", mp.get_script_name()) end)

-- every change mark of pause, also one that ends where it was: mpv reports nothing of a set
-- the person's key undid at once (ORDER.md)
mp.observe_property("pause", "none", function() say("pause") end)

-- set is the whole state the node adopts, in one message, so a person's key lands before it or
-- after it, never between its steps: stopped, on the entry, on the second, then playing if
-- asked. ack: once mpv has all of it, with what mpv holds then. A seek runs async: its reply
-- comes in this script's events in order with mpv's seek events, so the first seek after it
-- is the one that carries the node's. A move waits for its file to start (heading, loaded),
-- and a set meanwhile replaces the one waiting.
local replied, waiting, heading, loaded = {}, nil, nil, false
-- moves: the entries sets set, with their ids, oldest first, until a start says which opened;
-- ended: why the last file ended, so a start after a file's end or error is mpv's own (auto);
-- first: the file mpv opens as it starts is nobody's move
local moves, ended, first = {}, nil, true
-- over: the sets that undo a change of the person's the node hadn't read when it sent them, so
-- its ack says so; seeking: the set whose seek mpv has queued but not answered
local over, seeking = {}, nil

-- overrules: what mpv holds now, against what the node had read (seen), undone by a set that
-- ends on paused: the person's pause or seek the node never had. Another entry is the
-- person's key, unless mpv moved there itself at a file's end: its start said auto, or it is
-- still to come after that file's end.
local function overrules(seen, paused, entry)
    seen = tonumber(seen)
    local last = seen and said[seen]
    if not last then return false end
    local now = mp.get_property("pause")
    if now ~= last.pause and now ~= paused then return true end
    local own = ended == "eof" or ended == "error"
    for k = seen + 1, n do
        if said[k] and said[k].person then return true end
        if said[k] and said[k].auto then own = true end
    end
    local on = mp.get_property("playlist-pos")
    if entry ~= "-" and not own and on ~= last.entry and on ~= entry and on ~= heading then return true end
    return false
end

local function ack(id)
    local o = over[id]
    over[id] = nil
    say("ack", id, o)
end

local function apply(paused, second, id)
    local function finish()
        if paused == "no" then mp.set_property("pause", "no") end
        ack(id)
    end
    if second == "-" then
        mp.set_property("pause", paused)
        finish()
        return
    end
    mp.set_property("pause", "yes")
    seeking = id
    mp.command_native_async({"seek", second, "absolute+exact"}, function(ok)
        seeking = nil
        if not ok then
            over[id] = nil
            say("refused", id)
            return
        end
        table.insert(replied, id)
        -- playing before it is played: the person's space ran between the set's stop and here
        if paused == "no" and mp.get_property("pause") == "no" then over[id] = true end
        finish()
    end)
end

-- the set waiting, acked alone: mpv isn't on its file
local function drop()
    heading, loaded = nil, false
    local w = waiting
    waiting = nil
    if w then ack(w.id) end
end

mp.register_script_message("watchparty-set", function(paused, second, entry, id, seen, on)
    if overrules(seen, paused, entry) then over[id] = true end
    if waiting then
        ack(waiting.id)
        waiting = nil
    end
    -- a set for the file still starting waits for it too: a seek before it can be lost
    if entry == "-" and heading then entry = heading end
    -- for a file mpv has left by a key the node hasn't heard yet: only stopped, and the ack's
    -- entry sends that move out; a seek here would land on the wrong file
    if entry == "-" and on and on ~= "-" and mp.get_property("playlist-pos") ~= on then
        if paused == "yes" then mp.set_property("pause", "yes") end
        ack(id)
        return
    end
    -- toward: the entry mpv is on, or already heading to
    local toward = heading or mp.get_property("playlist-pos")
    if entry ~= "-" and (heading or toward ~= entry) then
        if toward ~= entry then
            mp.set_property("pause", "yes")
            heading, loaded = entry, false
            table.insert(moves, {entry = entry, id = id})
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
    if not heading then return end
    if opened ~= heading then
        -- another file: the set waits on if mpv is still on its way to its own
        if mp.get_property("playlist-pos") ~= heading then drop() end
        return
    end
    loaded = true
end)

-- the file started: a seek made at its file-loaded, before this, can be dropped on a stream
-- that is slow to start (ORDER.md)
mp.register_event("playback-restart", function()
    if not loaded then return end
    heading, loaded = nil, false
    local w = waiting
    waiting = nil
    if w then apply(w.paused, w.second, w.id) end
end)

-- a file mpv couldn't open: nothing loads, so the set waiting for it is acked
mp.register_event("end-file", function(e)
    ended = e.reason
    if e.reason == "error" and heading then drop() end
end)

-- seek: where a seek goes, read at its seek event, which is where it lands (ORDER.md), with
-- the node's id if it carries the node's
mp.register_event("seek", function()
    -- a seek before ours is answered: the person's, queued before it, and lost to it
    if seeking and #replied == 0 then over[seeking] = true end
    say("seek", replied[#replied])
    replied = {}
end)

-- a seek queued for the file mpv leaves never runs (MPV.md, "Playlists")
-- start: whose file mpv opens. A set's, by the move it set; mpv's own (auto) after a file's
-- end or error, or as it starts; else the person's (a key, the OSC, another script). A move
-- set before this start lost to it, and never opens; one set after it is still to come.
mp.register_event("start-file", function(e)
    replied = {}
    local entry = tostring((e.playlist_entry_id or 0) - 1)
    opened = entry
    local label = ""
    for i = 1, #moves do
        if moves[i].entry == entry then
            label = moves[i].id
            local rest = {}
            for k = i + 1, #moves do rest[#rest + 1] = moves[k] end
            moves = rest
            break
        end
    end
    if label == "" then
        local now, rest = mp.get_property("playlist-pos"), {}
        for _, m in ipairs(moves) do
            if m.entry == now then rest[#rest + 1] = m end
        end
        moves = rest
        if first or ended == "eof" or ended == "error" then
            label = "auto"
        elseif waiting and now == heading then
            -- the person's move, which the set's, set after it, undoes
            over[waiting.id] = true
        end
    end
    first, ended = false, nil
    say("start", label, false, entry)
end)
