const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const videos = [
 {id:'a',kind:'episode',path:'课程/基础/01.mp4'},
 {id:'b',kind:'episode',path:'课程/基础/进阶/02.mp4'},
 {id:'c',kind:'episode',path:'课程2/基础/01.mp4'},
 {id:'d',kind:'movie',path:'课程/独立.mp4'},
 {id:'e',kind:'episode',path:'根目录视频.mp4'},
];
const context = vm.createContext({state:{videos}});
vm.runInContext(fs.readFileSync('static/series.js','utf8'),context);
test('relative directory tree retains every level and distinguishes repeated names',()=>{
 const root = context.seriesTree(videos);
 assert.equal(root.members.length,4);
 assert.equal(root.direct[0].id,'e');
 assert.equal(root.children.size,2);
 const course=root.children.get('课程');
 assert.equal(course.members.length,2);
 assert.equal(course.direct.length,0);
 const basic=course.children.get('基础');
 assert.equal(basic.path,'课程/基础');
 assert.equal(basic.direct[0].id,'a');
 assert.equal(basic.children.get('进阶').direct[0].id,'b');
 assert.equal(root.children.get('课程2').children.get('基础').members.length,1);
});
test('directory selection includes descendants without matching sibling path prefixes or movies',()=>{
 assert.deepEqual(Array.from(context.seriesMembers('课程'),v=>v.id),['a','b']);
 assert.deepEqual(Array.from(context.seriesMembers('课程/基础/进阶'),v=>v.id),['b']);
 assert.equal(context.seriesMembers('.').length,4);
});
test('directory browsing separates this level from descendants and resolves full paths',()=>{
 const root = context.seriesTree(videos);
 assert.equal(context.directoryNode(root,'课程/基础/进阶').direct[0].id,'b');
 assert.equal(context.directoryNode(root,'missing'),null);
 assert.equal(context.inDirectory(videos[0],'课程/基础',false),true);
 assert.equal(context.inDirectory(videos[1],'课程/基础',false),false);
 assert.equal(context.inDirectory(videos[1],'课程/基础',true),true);
 assert.equal(context.inDirectory(videos[2],'课程',true),false);
 assert.equal(context.inDirectory(videos[4],'.',false),true);
 assert.equal(context.inDirectory(videos[0],'.',false),false);
});
test('paging clamps after filtering or deletion without dropping or repeating videos',()=>{
 const rows=Array.from({length:53},(_,id)=>({id}));
 const one=context.libraryPage(rows,1,25), two=context.libraryPage(rows,2,25), last=context.libraryPage(rows,99,25);
 assert.deepEqual([...one.videos,...two.videos,...last.videos].map(v=>v.id),rows.map(v=>v.id));
 assert.equal(last.page,3); assert.equal(last.start,50);
 const empty=context.libraryPage([],3,25); assert.equal(empty.page,1); assert.equal(empty.videos.length,0);
 assert.equal(context.libraryPage(rows.slice(0,8),3,25).page,1);
});
test('search combines path and metadata with status rather than losing the directory scope',()=>{
 const video={...videos[1], status:'error',metadata:{title:'Alphabet'},exists:false};
 assert.equal(context.matchesVideo(video,'alphabet','error'),true);
 assert.equal(context.matchesVideo(video,'进阶','unavailable'),true);
 assert.equal(context.matchesVideo(video,'alphabet','saved'),false);
 assert.equal(context.matchesVideo(video,'unrelated','all'),false);
});
