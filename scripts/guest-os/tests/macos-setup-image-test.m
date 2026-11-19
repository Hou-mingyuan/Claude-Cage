#import "../../../internal/maclogin/bridge_darwin.m"

static BOOL encodedOffMain, revokeDuringEncoding, replaceDuringEncoding;
@interface TestBitmap : NSBitmapImageRep
@end
@interface TestView : NSObject
@property BOOL valid;
- (NSBitmapImageRep *)snapshotBitmap;
- (BOOL)canUseView;
@end
@implementation TestBitmap
- (NSData *)representationUsingType:(NSBitmapImageFileType)type properties:(NSDictionary *)properties {
    encodedOffMain = !NSThread.isMainThread;
    if (revokeDuringEncoding) dispatch_sync(dispatch_get_main_queue(), ^{ ((TestView *)(id)loginView).valid = NO; });
    if (replaceDuringEncoding) dispatch_sync(dispatch_get_main_queue(), ^{
        TestView *replacement = [TestView new]; replacement.valid = YES;
        loginView = (CageGuestSetupView *)(id)replacement;
    });
    return [super representationUsingType:type properties:properties];
}
@end
@implementation TestView
- (BOOL)canUseView { NSCAssert(NSThread.isMainThread, @"authorization on main"); return self.valid; }
- (NSBitmapImageRep *)snapshotBitmap {
    NSCAssert(NSThread.isMainThread, @"capture on main");
    if (!self.valid) return nil;
    NSBitmapImageRep *rep = [[TestBitmap alloc] initWithBitmapDataPlanes:NULL pixelsWide:200 pixelsHigh:100
        bitsPerSample:8 samplesPerPixel:3 hasAlpha:NO isPlanar:NO colorSpaceName:NSCalibratedRGBColorSpace
        bytesPerRow:0 bitsPerPixel:0];
    memset(rep.bitmapData, 127, rep.bytesPerRow * rep.pixelsHigh);
    return rep;
}
@end

static void checkFrame(BOOL cropped, BOOL revoke, BOOL replace) {
    TestView *view = [TestView new]; view.valid = YES;
    loginView = (CageGuestSetupView *)(id)view;
    revokeDuringEncoding = revoke; replaceDuringEncoding = replace; encodedOffMain = NO;
    __block BOOL done = NO;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        @autoreleasepool {
            int size = 0;
            void *bytes = cropped ? cageLoginImageRegion(0, 0, 512, 320, &size) : cageLoginImage(&size);
            if (revoke || replace) {
                NSCAssert(bytes == NULL && size == 0, @"revoked frame must not escape");
            } else {
                NSCAssert(bytes != NULL && size > 0, @"JPEG returned");
                NSBitmapImageRep *image = [NSBitmapImageRep imageRepWithData:[NSData dataWithBytes:bytes length:size]];
                NSCAssert(image.pixelsWide == (cropped ? 100 : 200), @"crop width");
                NSCAssert(image.pixelsHigh == (cropped ? 50 : 100), @"crop height");
            }
            if (!cropped) NSCAssert(encodedOffMain, @"encoding must leave the input thread");
            free(bytes);
            dispatch_async(dispatch_get_main_queue(), ^{ done = YES; });
        }
    });
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:10];
    while (!done && deadline.timeIntervalSinceNow > 0)
        [NSRunLoop.currentRunLoop runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.01]];
    NSCAssert(done, @"frame request completed");
    loginView = nil;
}
int main(void) {
    @autoreleasepool {
        checkFrame(NO, NO, NO);
        checkFrame(YES, NO, NO);
        checkFrame(NO, YES, NO);
        checkFrame(NO, NO, YES);
        puts("setup frame dimensions, off-main encoding, revocation and replacement passed");
    }
}
