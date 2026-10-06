#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

static NSPanel *orbPanel = nil;
static WKWebView *orbWeb = nil;
static int orbW = 0, orbH = 0, orbMargin = 0;
static id orbScreenObserver = nil;
static NSTimer *orbGaze = nil;
static NSPoint orbLastMouse = {-1, -1};

// The panel only hears the mouse over itself, so while it shows, the
// character is told where the pointer is on the desktop (in the page's
// coordinates) and turns to look. Reading the position needs no permission,
// and nothing is sent while the pointer is still.
static void orb_gaze(BOOL on) {
    if (!on) {
        [orbGaze invalidate];
        orbGaze = nil;
        return;
    }
    if (orbGaze != nil) return;
    orbGaze = [NSTimer scheduledTimerWithTimeInterval:0.05 repeats:YES block:^(NSTimer *t) {
        if (orbPanel == nil || orbWeb == nil || !orbPanel.visible) return;
        NSPoint m = [NSEvent mouseLocation];
        if (NSEqualPoints(m, orbLastMouse)) return;
        orbLastMouse = m;
        NSRect f = orbPanel.frame;
        if (NSPointInRect(m, f)) return; // over the window, the page sees it itself
        NSString *js = [NSString stringWithFormat:@"window.orbPointer&&orbPointer(%.0f,%.0f)", m.x - NSMinX(f), NSMaxY(f) - m.y];
        [orbWeb evaluateJavaScript:js completionHandler:nil];
    }];
    orbGaze.tolerance = 0.02;
}

// orb_frame is the orb's place: the bottom right corner of the main screen
// as it is now (a display plugged in or out moves it).
static NSRect orb_frame(int w, int h, int margin) {
    NSScreen *scr = [NSScreen mainScreen];
    if (scr == nil) scr = [[NSScreen screens] firstObject];
    NSRect vf = scr ? scr.visibleFrame : NSMakeRect(0, 0, w + 2 * margin, h + 2 * margin);
    return NSMakeRect(NSMaxX(vf) - w - margin, NSMinY(vf) + margin, w, h);
}

static void orb_place(void) {
    if (orbPanel != nil) [orbPanel setFrame:orb_frame(orbW, orbH, orbMargin) display:YES];
}

static void orb_ensure(NSString *u, int w, int h, int margin) {
        orbW = w; orbH = h; orbMargin = margin;
        if (orbPanel == nil) {
            NSRect frame = orb_frame(w, h, margin);
            orbPanel = [[NSPanel alloc] initWithContentRect:frame
                                                  styleMask:(NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel)
                                                    backing:NSBackingStoreBuffered defer:NO];
            orbPanel.level = NSStatusWindowLevel;
            orbPanel.opaque = NO;
            orbPanel.backgroundColor = [NSColor clearColor];
            orbPanel.hasShadow = NO;
            orbPanel.hidesOnDeactivate = NO;
            orbPanel.floatingPanel = YES;
            orbPanel.becomesKeyOnlyIfNeeded = YES;
            orbPanel.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces | NSWindowCollectionBehaviorStationary | NSWindowCollectionBehaviorFullScreenAuxiliary;
            [orbPanel setIgnoresMouseEvents:YES];
            WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
            orbWeb = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, w, h) configuration:cfg];
            [orbWeb setValue:@NO forKey:@"drawsBackground"];
            orbWeb.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
            orbPanel.contentView = orbWeb;
            [orbWeb loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:u]]];
        }
        if (orbScreenObserver == nil) {
            orbScreenObserver = [[NSNotificationCenter defaultCenter]
                addObserverForName:NSApplicationDidChangeScreenParametersNotification
                            object:nil
                             queue:[NSOperationQueue mainQueue]
                        usingBlock:^(NSNotification *note) { orb_place(); }];
        }
}

void orb_preload(const char *curl, int w, int h, int margin) {
    NSString *u = [NSString stringWithUTF8String:curl];
    dispatch_async(dispatch_get_main_queue(), ^{ orb_ensure(u, w, h, margin); });
}

void orb_show(const char *curl, int w, int h, int margin) {
    NSString *u = [NSString stringWithUTF8String:curl];
    dispatch_async(dispatch_get_main_queue(), ^{
        orb_ensure(u, w, h, margin);
        orb_place(); // the screens may have changed since it was made
        [orbPanel setIgnoresMouseEvents:NO]; // showing: a click talks, or answers a request
        [orbPanel orderFrontRegardless];
        orb_gaze(YES);
    });
}

void orb_hide(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (orbPanel != nil) {
            [orbPanel setIgnoresMouseEvents:YES]; // hidden: clicks belong to the desktop
            [orbPanel orderOut:nil];
        }
        orb_gaze(NO);
    });
}

void orb_interactive(int on) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (orbPanel != nil) [orbPanel setIgnoresMouseEvents:(on ? NO : YES)];
    });
}

void orb_reload(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (orbWeb != nil) [orbWeb reload];
    });
}
