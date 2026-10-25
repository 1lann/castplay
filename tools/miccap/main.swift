// miccap captures mono audio from a macOS input device and writes it to stdout
// with a capture timestamp on every buffer, for castspike's latency
// measurements. ffmpeg's avfoundation input drops ~10% of buffers, which
// silently compresses the timeline; here each buffer carries the host time of
// its first sample, converted to Unix time, so gaps are explicit.
//
// Usage: miccap [device-name-substring]    (miccap -list to list inputs)
//
// Output: "MCAP" | f64 sampleRate | f64 inputLatencySeconds, then repeated
// i64 unixNanosOfFirstSample | u32 frames | frames × i16 (all little endian).
import AVFoundation
import CoreAudio
import Foundation

func inputDevices() -> [(AudioDeviceID, String)] {
    var addr = AudioObjectPropertyAddress(
        mSelector: kAudioHardwarePropertyDevices,
        mScope: kAudioObjectPropertyScopeGlobal,
        mElement: kAudioObjectPropertyElementMain)
    var size: UInt32 = 0
    AudioObjectGetPropertyDataSize(AudioObjectID(kAudioObjectSystemObject), &addr, 0, nil, &size)
    var ids = [AudioDeviceID](repeating: 0, count: Int(size) / MemoryLayout<AudioDeviceID>.size)
    AudioObjectGetPropertyData(AudioObjectID(kAudioObjectSystemObject), &addr, 0, nil, &size, &ids)
    var out: [(AudioDeviceID, String)] = []
    for id in ids {
        var streams = AudioObjectPropertyAddress(
            mSelector: kAudioDevicePropertyStreams,
            mScope: kAudioDevicePropertyScopeInput,
            mElement: kAudioObjectPropertyElementMain)
        var ssize: UInt32 = 0
        AudioObjectGetPropertyDataSize(id, &streams, 0, nil, &ssize)
        if ssize == 0 { continue }
        var nameAddr = AudioObjectPropertyAddress(
            mSelector: kAudioObjectPropertyName,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain)
        var name: Unmanaged<CFString>?
        var nsize = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
        AudioObjectGetPropertyData(id, &nameAddr, 0, nil, &nsize, &name)
        out.append((id, (name?.takeRetainedValue() as String?) ?? "?"))
    }
    return out
}

func fail(_ msg: String) -> Never {
    FileHandle.standardError.write((msg + "\n").data(using: .utf8)!)
    exit(1)
}

let args = CommandLine.arguments.dropFirst()
if args.first == "-list" {
    for (id, name) in inputDevices() { print("\(id)\t\(name)") }
    exit(0)
}

let engine = AVAudioEngine()
let input = engine.inputNode
if let want = args.first, !want.isEmpty, want != "default" {
    guard let dev = inputDevices().first(where: { $0.1.localizedCaseInsensitiveContains(want) }) else {
        fail("miccap: no input device matching \"\(want)\"")
    }
    var id = dev.0
    let st = AudioUnitSetProperty(
        input.audioUnit!, kAudioOutputUnitProperty_CurrentDevice, kAudioUnitScope_Global, 0,
        &id, UInt32(MemoryLayout<AudioDeviceID>.size))
    if st != noErr { fail("miccap: cannot select \(dev.1): \(st)") }
    FileHandle.standardError.write("miccap: using \(dev.1)\n".data(using: .utf8)!)
}

let format = input.outputFormat(forBus: 0)
var timebase = mach_timebase_info()
mach_timebase_info(&timebase)

var header = Data("MCAP".utf8)
withUnsafeBytes(of: format.sampleRate.bitPattern.littleEndian) { header.append(contentsOf: $0) }
withUnsafeBytes(of: input.presentationLatency.bitPattern.littleEndian) { header.append(contentsOf: $0) }
FileHandle.standardOutput.write(header)

signal(SIGPIPE, SIG_DFL)  // exit quietly when castspike stops reading

input.installTap(onBus: 0, bufferSize: 1024, format: format) { buf, when in
    let nowHost = mach_absolute_time()
    var ts = timespec()
    clock_gettime(CLOCK_REALTIME, &ts)
    let nowUnix = Int64(ts.tv_sec) * 1_000_000_000 + Int64(ts.tv_nsec)
    let ageTicks = Int64(nowHost) - Int64(when.hostTime)
    let ageNs = ageTicks * Int64(timebase.numer) / Int64(timebase.denom)
    let start = nowUnix - ageNs

    let frames = Int(buf.frameLength)
    guard let ch = buf.floatChannelData?[0] else { return }
    var out = Data(capacity: 12 + frames * 2)
    withUnsafeBytes(of: start.littleEndian) { out.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt32(frames).littleEndian) { out.append(contentsOf: $0) }
    for i in 0..<frames {
        let v = Int16(max(-1, min(1, ch[i])) * 32767)
        withUnsafeBytes(of: v.littleEndian) { out.append(contentsOf: $0) }
    }
    FileHandle.standardOutput.write(out)
}

do { try engine.start() } catch { fail("miccap: \(error)") }
dispatchMain()
