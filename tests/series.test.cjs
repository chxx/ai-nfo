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
