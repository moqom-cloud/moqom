// Render a string as a QR code: a PNG to scan from, and blocks to scan off a terminal.
//
// Here rather than `brew install qrencode` because the whole point of the thing
// it prints is that a phone can be set up with nothing else installed, and a
// setup path that begins with installing something has lost the argument. Every
// Mac that can build this repository already has CoreImage and a Swift compiler.
//
//   swift qr.swift 'moqom://join?...' /tmp/invite.png
//
// Correction level L, which is the lowest. A credential is four hundred
// characters and the correction level is what decides whether that fits in a
// code somebody can hold a phone up to: at L this is 73 modules across, and at Q
// it is 101. The redundancy is protection against a torn or dirty print, and
// this is a screen.

import CoreImage
import Foundation
import ImageIO
import UniformTypeIdentifiers

let arguments = CommandLine.arguments
guard arguments.count >= 2 else {
    FileHandle.standardError.write(Data("usage: qr.swift <text> [out.png]\n".utf8))
    exit(2)
}

guard let filter = CIFilter(name: "CIQRCodeGenerator") else { exit(1) }
filter.setValue(Data(arguments[1].utf8), forKey: "inputMessage")
filter.setValue("L", forKey: "inputCorrectionLevel")
guard let output = filter.outputImage else {
    FileHandle.standardError.write(Data("too long to encode\n".utf8))
    exit(1)
}

let context = CIContext()

// The modules, one pixel each, read back so they can be printed as text. The
// generator's own extent is exactly the module grid with no quiet zone, which is
// why one is added below rather than assumed: a code printed hard against
// surrounding text does not scan, and the failure looks like a bad code.
let width = Int(output.extent.width)
let height = Int(output.extent.height)
var pixels = [UInt8](repeating: 0, count: width * height)
guard
    let grey = CGColorSpace(name: CGColorSpace.linearGray),
    let bitmap = CGContext(
        data: &pixels,
        width: width,
        height: height,
        bitsPerComponent: 8,
        bytesPerRow: width,
        space: grey,
        bitmapInfo: CGImageAlphaInfo.none.rawValue
    ),
    let rendered = context.createCGImage(output, from: output.extent)
else { exit(1) }
bitmap.draw(rendered, in: CGRect(x: 0, y: 0, width: width, height: height))

if arguments.count >= 3 {
    // Scaled up on the way out, with no interpolation, because a 73-pixel PNG
    // opens as a postage stamp and a scaled one is what a camera can resolve.
    let scale = max(1, 512 / width)
    let big = output.transformed(by: CGAffineTransform(scaleX: CGFloat(scale), y: CGFloat(scale)))
    if
        let image = context.createCGImage(big, from: big.extent),
        let destination = CGImageDestinationCreateWithURL(
            URL(fileURLWithPath: arguments[2]) as CFURL, UTType.png.identifier as CFString, 1, nil
        )
    {
        CGImageDestinationAddImage(destination, image, nil)
        CGImageDestinationFinalize(destination)
    }
}

// Two module rows per line of text, as the top and bottom halves of one
// character. Colours are set explicitly rather than left to the terminal:
// a scanner needs dark modules on a light field, and half the world runs a dark
// terminal in which "print a space for white" produces a photographic negative
// that nothing will read.
let quiet = 4
func dark(_ x: Int, _ y: Int) -> Bool {
    let column = x - quiet
    let row = y - quiet
    guard column >= 0, column < width, row >= 0, row < height else { return false }
    // The bitmap's origin is bottom-left and the code reads top-down.
    return pixels[(height - 1 - row) * width + column] < 128
}

let white = "\u{1B}[97m"
let onWhite = "\u{1B}[107m"
let black = "\u{1B}[30m"
let onBlack = "\u{1B}[40m"
let reset = "\u{1B}[0m"

var out = ""
for y in stride(from: 0, to: height + quiet * 2, by: 2) {
    for x in 0 ..< (width + quiet * 2) {
        out += (dark(x, y) ? black : white) + (dark(x, y + 1) ? onBlack : onWhite) + "\u{2580}"
    }
    out += reset + "\n"
}
print(out, terminator: "")
