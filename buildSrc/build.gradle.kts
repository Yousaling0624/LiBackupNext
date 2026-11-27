plugins {
    `java-library`
}

// 只用 Gradle 自带的 JDK 编译，不引入任何依赖：buildSrc 编译慢一些，
// 但比起在 build.gradle.kts 里写 doLast 闭包，能保持 configuration cache 可用。
java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

dependencies {
    // buildSrc 是普通 Java 项目，Gradle API 不会自动出现（kotlin-dsl 插件才会加）
    implementation(gradleApi())
}
