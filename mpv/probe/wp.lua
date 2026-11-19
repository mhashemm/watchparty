-- probe: pause as one ordered stream. Our sets come in by script-message; every change mark
-- of pause (observed "none": no value comparison) goes out as an event with the value read now.
mp.observe_property("pause", "none", function()
    mp.commandv("script-message", "wp-pause", mp.get_property("pause"))
end)
mp.register_script_message("wp-set-pause", function(v, id)
    mp.set_property("pause", v)
    mp.commandv("script-message", "wp-ack", id, mp.get_property("pause"))
end)

-- Seeks. Our seek runs async, so its reply sits in this script's event queue in order with mpv's
-- seek events: the first seek event after the reply is the seek that carries ours (folded with
-- the person's, if they pressed a key in between). Every landing is sent, with our id when it
-- ends the seek that carried ours; the node compares the position with its target.
local replied, carrying = {}, nil
mp.register_script_message("wp-seek", function(target, id)
    mp.command_native_async({"seek", target, "absolute+exact"}, function(ok)
        if ok then table.insert(replied, id) end
    end)
end)
mp.register_event("seek", function()
    if #replied > 0 then
        carrying = replied[#replied]
        replied = {}
    end
end)
mp.register_event("playback-restart", function()
    mp.commandv("script-message", "wp-landed", string.format("%.3f", mp.get_property_number("time-pos", -1)), carrying or "")
    carrying = nil
end)
